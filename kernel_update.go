// kernel_update: online kernel status and updater for Debian/Ubuntu and EL-family x86_64.
// Official repository metadata is fetched directly; local package caches are not used
// to determine the latest available kernel. Installation/removal uses the native package manager.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
	"github.com/klauspost/compress/zstd"
)

const (
	requestTimeout = 15 * time.Second
	appVersion     = "1.0.0"
	red            = "\033[0;91m"
	green          = "\033[0;92m"
	yellow         = "\033[0;93m"
	reset          = "\033[0m"
)

func showLogo() {
	logo := []string{
		` _    _  __ _   _ `,
		`| |  | |/ /| | | |`,
		`| |  | ' / | | | |`,
		`| |__| . \ | |_| |`,
		`|____|_|\_\ \___/ `,
	}
	for _, line := range logo {
		fmt.Println(line)
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("\n%sv%s%s (Linux kernel updater)\n\n", yellow, appVersion, reset)
}

type spinner struct {
	stop chan struct{}
	done chan struct{}
}

func startSpinner() *spinner {
	s := &spinner{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		frames := []string{"--", "\\", "|", "/"}
		i := 0
		fmt.Printf("\rWorking %s ", frames[i])
		for {
			select {
			case <-s.stop:
				fmt.Printf("\rWorking     \r")
				close(s.done)
				return
			case <-time.After(120 * time.Millisecond):
				i = (i + 1) % len(frames)
				fmt.Printf("\rWorking %s ", frames[i])
			}
		}
	}()
	return s
}

func printError(msg string) {
	fmt.Printf("%sError%s %s\n", red, reset, msg)
}

func printWarning(msg string) {
	fmt.Printf("%sWarning%s %s\n", yellow, reset, msg)
}

func printSuccess(version string) {
	fmt.Printf("Update to %s %ssuccess%s\n", version, green, reset)
}

func printFailed(version string, err error) {
	fmt.Printf("Update to %s %sfailed%s\n", version, red, reset)
	if err != nil {
		msg := err.Error()
		if msg != "" {
			fmt.Println(msg)
		}
	}
}

func printReboot() {
	fmt.Println("Reboot required to start the new kernel.")
}

// confirmYesNo reads a single key without Enter and returns true for y/н.
func confirmYesNo(prompt string) bool {
	fmt.Print(prompt)
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		var answer string
		fmt.Scanln(&answer)
		a := strings.ToLower(strings.TrimSpace(answer))
		return a == "y" || a == "н" || a == "yes" || a == "да"
	}
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		var answer string
		fmt.Scanln(&answer)
		a := strings.ToLower(strings.TrimSpace(answer))
		return a == "y" || a == "н" || a == "yes" || a == "да"
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	buf := make([]byte, 4)
	n, err := os.Stdin.Read(buf)
	if err != nil || n == 0 {
		fmt.Print("\r\n")
		return false
	}
	if buf[0] == 3 { // Ctrl+C
		fmt.Print("\r\n")
		os.Exit(130)
	}
	input := strings.ToLower(strings.TrimSpace(string(buf[:n])))
	yes := input == "y" || input == "н"
	if yes {
		fmt.Print("y\r\n")
	} else {
		fmt.Print("N\r\n")
	}
	return yes
}

func (s *spinner) Stop() {
	close(s.stop)
	<-s.done
	fmt.Print("\r\033[K")
}

type Release struct {
	Version string `json:"version"`
}
type KernelOrg struct {
	LatestStable Release   `json:"latest_stable"`
	Mainline     Release   `json:"mainline"`
	Stable       []Release `json:"stable"`
	Longterm     []Release `json:"longterm"`
}
type OSInfo struct{ ID, Version, Codename string }
type Repo struct{ URI, Suite, Component string }
type eolCycle struct {
	Cycle string `json:"cycle"`
	Eol   string `json:"eol"`
}

// eolProductID maps OSInfo.ID to endoflife.date product slug.
func eolProductID(id string) string {
	switch id {
	case "debian":
		return "debian"
	case "ubuntu":
		return "ubuntu"
	case "rhel":
		return "rhel"
	case "centos":
		return "centos"
	case "rocky":
		return "rocky-linux"
	case "almalinux":
		return "almalinux"
	case "ol":
		return "oracle-linux"
	}
	return ""
}

// checkEOL queries endoflife.date (v0 JSON API) and returns (eol bool, eolDate string, err error).
func checkEOL(oi OSInfo) (bool, string, error) {
	product := eolProductID(oi.ID)
	if product == "" {
		return false, "", nil
	}
	u := "https://endoflife.date/api/" + product + ".json"
	if verboseMode {
		fmt.Printf("[eol] query %s\n", u)
	}
	b, err := get(u)
	if err != nil {
		return false, "", fmt.Errorf("endoflife.date query failed: %w", err)
	}
	var cycles []eolCycle
	if err := json.Unmarshal(b, &cycles); err != nil {
		return false, "", fmt.Errorf("endoflife.date parse failed: %w", err)
	}
	ver := strings.TrimSpace(oi.Version)
	if verboseMode {
		fmt.Printf("[eol] product=%s version=%q cycles=%d\n", product, ver, len(cycles))
	}
	for _, c := range cycles {
		if verboseMode {
			fmt.Printf("[eol]   cycle=%q eol=%q\n", c.Cycle, c.Eol)
		}
		if c.Cycle != ver && !strings.HasPrefix(ver, c.Cycle+".") {
			continue
		}
		if c.Eol == "" {
			return false, "", nil
		}
		t, err := time.Parse("2006-01-02", c.Eol)
		if err != nil {
			return false, "", nil
		}
		return time.Now().After(t), c.Eol, nil
	}
	return false, "", nil
}

var client *http.Client
var verboseMode bool
var chrootMode bool

func command(name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	b, err := c.CombinedOutput()
	if err != nil {
		return string(b), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}

func httpClient(proxy string) (*http.Client, error) {
	tlsConfig := &tls.Config{InsecureSkipVerify: true}
	// RHEL subscription CDN requires the entitlement client certificate.
	certs, _ := filepath.Glob("/etc/pki/entitlement/*.pem")
	for _, cert := range certs {
		if strings.HasSuffix(cert, "-key.pem") {
			continue
		}
		key := strings.TrimSuffix(cert, ".pem") + "-key.pem"
		if _, e := os.Stat(key); e == nil {
			if pair, e := tls.LoadX509KeyPair(cert, key); e == nil {
				tlsConfig.Certificates = append(tlsConfig.Certificates, pair)
				break
			}
		}
	}
	t := &http.Transport{TLSClientConfig: tlsConfig, Proxy: http.ProxyFromEnvironment}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, err
		}
		if u.Scheme == "socks5" || u.Scheme == "socks5h" {
			host := u.Host
			if !strings.Contains(host, ":") {
				host += ":1080"
			}
			d := &net.Dialer{Timeout: requestTimeout}
			t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return socks5Dial(ctx, d, host, address, u)
			}
			t.Proxy = nil
		} else {
			t.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: t, Timeout: requestTimeout}, nil
}

// Minimal SOCKS5 CONNECT client; supports no-auth and username/password.
func socks5Dial(ctx context.Context, d *net.Dialer, proxy, target string, pu *url.URL) (net.Conn, error) {
	c, err := d.DialContext(ctx, "tcp", proxy)
	if err != nil {
		return nil, err
	}
	fail := func(e error) (net.Conn, error) { c.Close(); return nil, e }
	methods := []byte{5, 1, 0}
	if pu.User != nil {
		methods = []byte{5, 2, 0, 2}
	}
	if _, err = c.Write(methods); err != nil {
		return fail(err)
	}
	var h [2]byte
	if _, err = io.ReadFull(c, h[:]); err != nil {
		return fail(err)
	}
	if h[0] != 5 || h[1] == 255 {
		return fail(errors.New("SOCKS5 authentication method rejected"))
	}
	if h[1] == 2 {
		user := pu.User.Username()
		pass, _ := pu.User.Password()
		if len(user) > 255 || len(pass) > 255 {
			return fail(errors.New("SOCKS5 credentials too long"))
		}
		b := []byte{1, byte(len(user))}
		b = append(b, []byte(user)...)
		b = append(b, byte(len(pass)))
		b = append(b, []byte(pass)...)
		if _, err = c.Write(b); err != nil {
			return fail(err)
		}
		if _, err = io.ReadFull(c, h[:]); err != nil {
			return fail(err)
		}
		if h[1] != 0 {
			return fail(errors.New("SOCKS5 authentication failed"))
		}
	} else if h[1] != 0 {
		return fail(errors.New("unsupported SOCKS5 authentication"))
	}
	host, portS, err := net.SplitHostPort(target)
	if err != nil {
		return fail(err)
	}
	port, e := strconv.Atoi(portS)
	if e != nil || port < 1 || port > 65535 {
		return fail(errors.New("invalid target port"))
	}
	ip := net.ParseIP(host)
	req := []byte{5, 1, 0}
	if ip4 := ip.To4(); ip4 != nil {
		req = append(req, 1)
		req = append(req, ip4...)
	} else if ip6 := ip.To16(); ip6 != nil {
		req = append(req, 4)
		req = append(req, ip6...)
	} else {
		if len(host) > 255 {
			return fail(errors.New("target hostname too long"))
		}
		req = append(req, 3, byte(len(host)))
		req = append(req, []byte(host)...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err = c.Write(req); err != nil {
		return fail(err)
	}
	var r [4]byte
	if _, err = io.ReadFull(c, r[:]); err != nil {
		return fail(err)
	}
	if r[1] != 0 {
		return fail(fmt.Errorf("SOCKS5 connect failed: %d", r[1]))
	}
	var n int
	switch r[3] {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		var z [1]byte
		if _, err = io.ReadFull(c, z[:]); err != nil {
			return fail(err)
		}
		n = int(z[0])
	default:
		return fail(errors.New("invalid SOCKS5 response"))
	}
	if _, err = io.CopyN(io.Discard, c, int64(n+2)); err != nil {
		return fail(err)
	}
	return c, nil
}

func get(url string) ([]byte, error) {
	if client == nil {
		return nil, errors.New("HTTP client is not initialized")
	}
	if verboseMode {
		fmt.Printf("HTTP GET %s\n", url)
	}
	var last error
	for i := 0; i < 3; i++ {
		r, err := client.Get(url)
		if err == nil {
			b, e := io.ReadAll(io.LimitReader(r.Body, 128<<20))
			r.Body.Close()
			if e == nil && r.StatusCode >= 200 && r.StatusCode < 300 {
				return b, nil
			}
			if e != nil {
				err = e
			} else {
				err = fmt.Errorf("HTTP %s", r.Status)
			}
		}
		last = err
		time.Sleep(time.Duration(i+1) * time.Second)
	}
	return nil, last
}

// readOS reads distribution identity exclusively from /etc/*release* files.
func readOS() (OSInfo, error) {
	candidates := []string{"/etc/os-release", "/etc/*release*"}
	var files []string
	for _, c := range candidates {
		matches, _ := filepath.Glob(c)
		files = append(files, matches...)
	}
	seen := map[string]bool{}
	m := map[string]string{}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		b, e := os.ReadFile(f)
		if e != nil {
			continue
		}
		for _, l := range strings.Split(string(b), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			p := strings.SplitN(l, "=", 2)
			if len(p) != 2 {
				continue
			}
			k := strings.TrimSpace(p[0])
			v := strings.Trim(strings.TrimSpace(p[1]), "\"'")
			if _, ok := m[k]; !ok {
				m[k] = v
			}
		}
	}
	id := m["ID"]
	if id == "" {
		id = strings.ToLower(m["DISTRIB_ID"])
	}
	ver := m["VERSION_ID"]
	if ver == "" {
		ver = m["DISTRIB_RELEASE"]
	}
	code := m["VERSION_CODENAME"]
	if code == "" {
		code = m["DISTRIB_CODENAME"]
	}
	if id == "" {
		return OSInfo{}, errors.New("Sorry, cannot detect this OS")
	}
	return OSInfo{id, ver, code}, nil
}

func parseRepos() []Repo {
	files, _ := filepath.Glob("/etc/apt/sources.list.d/*.sources")
	listFiles, _ := filepath.Glob("/etc/apt/sources.list.d/*.list")
	files = append(files, listFiles...)
	files = append(files, "/etc/apt/sources.list")
	var out []Repo
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		s := string(b)
		if strings.HasSuffix(f, ".sources") {
			for _, stanza := range strings.Split(s, "\n\n") {
				fields := map[string]string{}
				for _, line := range strings.Split(stanza, "\n") {
					p := strings.SplitN(line, ":", 2)
					if len(p) == 2 {
						fields[strings.TrimSpace(p[0])] = strings.TrimSpace(p[1])
					}
				}
				if strings.EqualFold(fields["Enabled"], "no") {
					continue
				}
				for _, uri := range strings.Fields(fields["URIs"]) {
					for _, suite := range strings.Fields(fields["Suites"]) {
						for _, component := range strings.Fields(fields["Components"]) {
							out = append(out, Repo{uri, suite, component})
						}
					}
				}
			}
		} else {
			sc := bufio.NewScanner(strings.NewReader(s))
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if !strings.HasPrefix(line, "deb ") {
					continue
				}
				p := strings.Fields(line)
				i := 1
				if i < len(p) && strings.HasPrefix(p[i], "[") {
					for i < len(p) && !strings.HasSuffix(p[i], "]") {
						i++
					}
					i++
				}
				if len(p) < i+3 {
					continue
				}
				for _, component := range p[i+2:] {
					out = append(out, Repo{p[i], p[i+1], component})
				}
			}
		}
	}
	return out
}
func packagesURL(r Repo) string {
	return strings.TrimRight(r.URI, "/") + "/dists/" + r.Suite + "/" + r.Component + "/binary-amd64/Packages.gz"
}
func packageRecords(data []byte) []map[string]string {
	z, e := gzip.NewReader(strings.NewReader(string(data)))
	if e != nil {
		return nil
	}
	defer z.Close()
	raw, _ := io.ReadAll(z)
	var out []map[string]string
	for _, rec := range strings.Split(string(raw), "\n\n") {
		m := map[string]string{}
		key := ""
		for _, l := range strings.Split(rec, "\n") {
			if strings.HasPrefix(l, " ") && key != "" {
				m[key] += " " + strings.TrimSpace(l)
				continue
			}
			p := strings.SplitN(l, ":", 2)
			if len(p) == 2 {
				key = p[0]
				m[key] = strings.TrimSpace(p[1])
			}
		}
		if m["Package"] != "" {
			out = append(out, m)
		}
	}
	return out
}
func officialAPTURI(uri, id string) bool {
	u, e := url.Parse(uri)
	if e != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	domains := []string{}
	if id == "debian" {
		domains = []string{"debian.org"}
	} else if id == "ubuntu" {
		domains = []string{"ubuntu.com"}
	}
	for _, d := range domains {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}
func debVersionGreater(a, b string) bool {
	if b == "" {
		return true
	}
	_, e := command("dpkg", "--compare-versions", a, "gt", b)
	return e == nil
}

// officialAPTRepos returns the official repositories for the detected distro.
func officialAPTRepos(osinfo OSInfo) []Repo {
	if osinfo.ID == "debian" {
		suite := osinfo.Codename
		if suite == "" {
			suite = osinfo.Version
		}
		return []Repo{
			{"http://deb.debian.org/debian", suite, "main"},
			{"http://security.debian.org/debian-security", suite + "-security", "main"},
		}
	}
	if osinfo.ID == "ubuntu" {
		suite := osinfo.Codename
		if suite == "" {
			suite = osinfo.Version
		}
		return []Repo{
			{"http://archive.ubuntu.com/ubuntu", suite, "main"},
			{"http://security.ubuntu.com/ubuntu", suite + "-security", "main"},
		}
	}
	return nil
}

func latestDistroKernel(osinfo OSInfo) (string, string, error) {
	meta := "linux-image-amd64"
	if osinfo.ID == "ubuntu" {
		meta = "linux-image-generic"
	} else if osinfo.ID != "debian" {
		return "", "", fmt.Errorf("unsupported distribution: %s", osinfo.ID)
	}
	repos := officialAPTRepos(osinfo)
	if len(repos) == 0 {
		return "", "", fmt.Errorf("no official repositories defined for %s", osinfo.ID)
	}
	if verboseMode {
		fmt.Printf("[apt] using %d official repo entries\n", len(repos))
		for _, r := range repos {
			fmt.Printf("[apt]   uri=%s suite=%s component=%s\n", r.URI, r.Suite, r.Component)
		}
	}
	bestMeta, bestImage := "", ""
	var errs []string
	for _, r := range repos {
		u := packagesURL(r)
		if verboseMode {
			fmt.Printf("[apt] fetch %s\n", u)
		}
		b, e := get(u)
		if e != nil {
			errs = append(errs, e.Error())
			if verboseMode {
				fmt.Printf("[apt] fetch failed: %v\n", e)
			}
			continue
		}
		recs := packageRecords(b)
		if verboseMode {
			fmt.Printf("[apt] %d package records from %s\n", len(recs), u)
		}
		found := 0
		for _, p := range recs {
			if p["Package"] != meta {
				continue
			}
			found++
			v := p["Version"]
			dep := p["Depends"]
			re := regexp.MustCompile(`linux-image-[0-9][^ ,|()]*`)
			img := re.FindString(dep)
			if img == "" && osinfo.ID == "debian" {
				img = "linux-image-amd64"
			}
			if verboseMode {
				fmt.Printf("[apt] candidate meta=%s version=%s image=%s\n", meta, v, img)
			}
			if !debVersionGreater(v, bestMeta) {
				continue
			}
			bestMeta, bestImage = v, img
		}
		if found == 0 && verboseMode {
			fmt.Printf("[apt] no %s in %s\n", meta, u)
		}
	}
	if verboseMode {
		fmt.Printf("[apt] best meta=%s image=%s\n", bestMeta, bestImage)
	}
	if bestImage == "" {
		return "", "", fmt.Errorf("cannot resolve %s from official online package indexes (%s)", meta, strings.Join(errs, "; "))
	}
	return bestMeta, bestImage, nil
}
func kernelOrgVersion() string {
	b, e := get("https://www.kernel.org/releases.json")
	if e != nil {
		return "unavailable: " + e.Error()
	}
	var k KernelOrg
	if json.Unmarshal(b, &k) != nil {
		return "unavailable: invalid releases.json"
	}
	return k.LatestStable.Version
}
func installedKernelPackages() []string {
	out, _ := command("dpkg-query", "-W", "-f=${binary:Package}\n", "linux-image-[0-9]*")
	var p []string
	for _, l := range strings.Fields(out) {
		p = append(p, l)
	}
	return p
}
func isContainer() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv", "/proc/vz"} {
		if _, e := os.Stat(p); e == nil {
			return true
		}
	}
	b, _ := os.ReadFile("/proc/1/cgroup")
	s := strings.ToLower(string(b))
	return strings.Contains(s, "docker") || strings.Contains(s, "lxc") || strings.Contains(s, "kubepods") || strings.Contains(s, "containerd") || strings.Contains(s, "openvz")
}
func latestInstalledDebianKernel() string {
	out, _ := command("dpkg-query", "-W", "-f=${binary:Package}\n", "linux-image-[0-9]*")
	best := ""
	for _, p := range strings.Fields(out) {
		p = strings.TrimSuffix(p, ":amd64")
		v := strings.TrimPrefix(p, "linux-image-")
		if v != p && (best == "" || debVersionGreater(v, best)) {
			best = v
		}
	}
	return best
}
func isChroot() bool {
	root, err := os.Stat("/")
	if err != nil {
		return false
	}
	pid1Root, err := os.Stat("/proc/1/root")
	if err != nil {
		return false
	}
	return !os.SameFile(root, pid1Root)
}
func runApt(args ...string) error {
	out, e := command("apt-get", args...)
	if out != "" {
		fmt.Print(out)
	}
	return e
}

type RPMRepo struct {
	Name, BaseURL, MirrorList, Metalink string
	Enabled                             bool
}
type Repomd struct {
	Data []struct {
		Type     string `xml:"type,attr"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
	} `xml:"data"`
}
type RPMPrimary struct {
	Packages []struct {
		Name    string `xml:"name"`
		Arch    string `xml:"arch"`
		Version struct {
			Epoch string `xml:"epoch,attr"`
			Ver   string `xml:"ver,attr"`
			Rel   string `xml:"rel,attr"`
		} `xml:"version"`
	} `xml:"package"`
}
type RPMCandidate struct{ Name, EVR, Arch, Repo string }

func parseRPMRepos() []RPMRepo {
	files, _ := filepath.Glob("/etc/yum.repos.d/*.repo")
	var out []RPMRepo
	for _, f := range files {
		b, e := os.ReadFile(f)
		if e != nil {
			continue
		}
		section := ""
		cur := RPMRepo{}
		flush := func() {
			if section != "" && cur.Enabled {
				out = append(out, cur)
			}
		}
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";") {
				continue
			}
			if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
				flush()
				section = l[1 : len(l)-1]
				cur = RPMRepo{Name: section, Enabled: true}
				continue
			}
			p := strings.SplitN(l, "=", 2)
			if len(p) != 2 {
				continue
			}
			k, v := strings.ToLower(strings.TrimSpace(p[0])), strings.TrimSpace(p[1])
			switch k {
			case "baseurl":
				cur.BaseURL = v
			case "mirrorlist":
				cur.MirrorList = v
			case "metalink":
				cur.Metalink = v
			case "enabled":
				cur.Enabled = v != "0"
			}
		}
		flush()
	}
	return out
}
func officialRPMHost(u string, id string) bool {
	x, e := url.Parse(u)
	if e != nil {
		return false
	}
	h := strings.ToLower(x.Hostname())
	allowed := map[string][]string{
		"rhel": {"redhat.com", "access.redhat.com"}, "rocky": {"rockylinux.org"}, "almalinux": {"almalinux.org"}, "centos": {"centos.org"}, "ol": {"oracle.com"}, "fedora": {"fedoraproject.org", "getfedora.org"},
	}
	for _, d := range allowed[id] {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	// Official CDN aliases used by the named distributions.
	if id == "rhel" && (strings.Contains(h, "cdn.redhat.com") || strings.Contains(h, "rhui")) {
		return true
	}
	return false
}
func osVersionFallback(id string) string {
	b, _ := os.ReadFile("/etc/os-release")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VERSION_ID=") {
			return strings.Trim(l[len("VERSION_ID="):], "\"'")
		}
	}
	return "0"
}
func resolveRPMBase(r RPMRepo, id string) ([]string, error) {
	var bases []string
	major := strings.SplitN(strings.TrimPrefix(osVersionFallback(id), ""), ".", 2)[0]
	release := major
	for _, f := range []string{"/etc/dnf/vars/releasever", "/etc/yum/vars/releasever"} {
		if b, e := os.ReadFile(f); e == nil && strings.TrimSpace(string(b)) != "" {
			release = strings.TrimSpace(string(b))
			break
		}
	}
	repl := strings.NewReplacer("${releasever}", release, "$releasever", release, "${basearch}", "x86_64", "$basearch", "x86_64", "${arch}", "x86_64", "$arch", "x86_64", "${stream}", major, "$stream", major, "${contentdir}", id, "$contentdir", id)
	r.BaseURL = repl.Replace(r.BaseURL)
	r.MirrorList = repl.Replace(r.MirrorList)
	r.Metalink = repl.Replace(r.Metalink)
	if r.BaseURL != "" {
		for _, v := range strings.Fields(r.BaseURL) {
			if officialRPMHost(v, id) {
				bases = append(bases, v)
			}
		}
	}
	fetchList := func(u string) {
		b, e := get(u)
		if e != nil {
			return
		}
		for _, l := range strings.Split(string(b), "\n") {
			v := strings.TrimSpace(l)
			if strings.HasPrefix(v, "http") {
				bases = append(bases, v)
			}
		}
	}
	if len(bases) == 0 && r.MirrorList != "" && officialRPMHost(r.MirrorList, id) {
		fetchList(r.MirrorList)
	}
	if len(bases) == 0 && r.Metalink != "" && officialRPMHost(r.Metalink, id) {
		b, e := get(r.Metalink)
		if e == nil {
			var x struct {
				URLs []string `xml:"files>file>resources>url"`
			}
			if xml.Unmarshal(b, &x) == nil {
				for _, v := range x.URLs {
					u := strings.TrimSpace(v)
					if officialRPMHost(u, id) {
						bases = append(bases, u)
					}
				}
			}
		}
	}
	if len(bases) == 0 {
		return nil, fmt.Errorf("repo %s: no official baseurl/mirror found", r.Name)
	}
	return bases, nil
}
func rpmPrimary(base string) ([]RPMCandidate, error) {
	rb, e := get(strings.TrimRight(base, "/") + "/repodata/repomd.xml")
	if e != nil {
		return nil, e
	}
	var md Repomd
	if e = xml.Unmarshal(rb, &md); e != nil {
		return nil, e
	}
	href := ""
	for _, d := range md.Data {
		if d.Type == "primary" {
			href = d.Location.Href
			break
		}
	}
	if href == "" {
		return nil, errors.New("primary metadata not found")
	}
	u := href
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = strings.TrimRight(base, "/") + "/" + strings.TrimLeft(href, "/")
	}
	data, e := get(u)
	if e != nil {
		return nil, e
	}
	if strings.HasSuffix(href, ".gz") {
		z, er := gzip.NewReader(bytes.NewReader(data))
		if er != nil {
			return nil, er
		}
		defer z.Close()
		data, er = io.ReadAll(z)
		if er != nil {
			return nil, er
		}
	} else if strings.HasSuffix(href, ".zst") || strings.HasSuffix(href, ".zstd") {
		z, er := zstd.NewReader(bytes.NewReader(data))
		if er != nil {
			return nil, fmt.Errorf("zstd decompression failed: %w", er)
		}
		data, er = io.ReadAll(z)
		z.Close()
		if er != nil {
			return nil, er
		}
	} else if !strings.HasSuffix(href, ".xml") {
		return nil, fmt.Errorf("unsupported primary metadata compression: %s", href)
	}
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	var out []RPMCandidate
	for {
		tok, er := dec.Token()
		if er == io.EOF {
			break
		}
		if er != nil {
			return nil, er
		}
		st, ok := tok.(xml.StartElement)
		if !ok || st.Name.Local != "package" {
			continue
		}
		var p struct {
			Name    string `xml:"name"`
			Arch    string `xml:"arch"`
			Version struct {
				Epoch string `xml:"epoch,attr"`
				Ver   string `xml:"ver,attr"`
				Rel   string `xml:"rel,attr"`
			} `xml:"version"`
		}
		if er = dec.DecodeElement(&p, &st); er != nil {
			continue
		}
		if p.Arch != "x86_64" && p.Arch != "noarch" {
			continue
		}
		epoch := p.Version.Epoch
		if epoch == "(none)" || epoch == "" {
			epoch = "0"
		}
		out = append(out, RPMCandidate{p.Name, epoch + ":" + p.Version.Ver + "-" + p.Version.Rel, p.Arch, base})
	}
	return out, nil
}
func rpmvercmp(a, b string) int { // rpm-style segment comparison
	for a != "" || b != "" {
		for len(a) > 0 && (a[0] == '~' || a[0] == '^' || !isAlphaNum(a[0])) {
			if a[0] == '~' {
				if b == "" || b[0] != '~' {
					return -1
				}
				a = a[1:]
				b = b[1:]
				continue
			}
			a = a[1:]
		}
		for len(b) > 0 && (b[0] == '~' || b[0] == '^' || !isAlphaNum(b[0])) {
			if b[0] == '~' {
				return 1
			}
			b = b[1:]
		}
		if a == "" || b == "" {
			break
		}
		if a[0] == '~' || b[0] == '~' {
			continue
		}
		numeric := a[0] >= '0' && a[0] <= '9'
		if numeric != (b[0] >= '0' && b[0] <= '9') {
			if numeric {
				return 1
			}
			return -1
		}
		i, j := 0, 0
		for i < len(a) && isAlphaNum(a[i]) && ((a[i] >= '0' && a[i] <= '9') == numeric) {
			i++
		}
		for j < len(b) && isAlphaNum(b[j]) && ((b[j] >= '0' && b[j] <= '9') == numeric) {
			j++
		}
		x, y := a[:i], b[:j]
		if numeric {
			x = strings.TrimLeft(x, "0")
			y = strings.TrimLeft(y, "0")
			if len(x) > len(y) {
				return 1
			}
			if len(x) < len(y) {
				return -1
			}
		}
		if x > y {
			return 1
		}
		if x < y {
			return -1
		}
		a = a[i:]
		b = b[j:]
	}
	if a == "" && b == "" {
		return 0
	}
	if a == "" {
		return -1
	}
	return 1
}
func isAlphaNum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
func isOfficialRPMRepo(r RPMRepo, id string) bool {
	if officialRPMHost(r.BaseURL, id) || officialRPMHost(r.MirrorList, id) || officialRPMHost(r.Metalink, id) {
		return true
	}
	// DNF may have third-party repositories enabled alongside the distribution repositories.
	// Never let EPEL or other third-party repos turn an official-kernel check into an error.
	name := strings.ToLower(r.Name)
	if strings.Contains(name, "epel") || strings.Contains(name, "elrepo") || strings.Contains(name, "remi") || strings.Contains(name, "rpmfusion") {
		return false
	}
	return false
}

func latestRPMKernel(osinfo OSInfo) (RPMCandidate, string, error) {
	repos := parseRPMRepos()
	if len(repos) == 0 {
		return RPMCandidate{}, "", errors.New("enabled RPM repositories not found")
	}
	pkg := "kernel-core"
	install := "kernel"
	if osinfo.ID == "ol" { // Oracle UEK is preferred only when UEK repo is configured and publishes UEK packages.
		pkg = "kernel-uek-core"
		install = "kernel-uek"
	}
	var best RPMCandidate
	var errs []string
	for _, r := range repos {
		if !isOfficialRPMRepo(r, osinfo.ID) {
			continue
		}
		bases, e := resolveRPMBase(r, osinfo.ID)
		if verboseMode {
			fmt.Printf("[rpm] %s -> %d base urls\n", r.Name, len(bases))
			for _, b := range bases {
				fmt.Printf("[rpm]   %s\n", b)
			}
		}
		if e != nil {
			errs = append(errs, e.Error())
			continue
		}
		for _, base := range bases {
			ps, e := rpmPrimary(base)
			if e != nil {
				errs = append(errs, e.Error())
				continue
			}
			for _, p := range ps {
				if p.Name != pkg {
					continue
				}
				if best.EVR == "" || rpmvercmp(p.EVR, best.EVR) > 0 {
					best = p
					best.Repo = r.Name
				}
			}
		}
	}
	if best.EVR == "" {
		return best, "", fmt.Errorf("cannot find %s in official online RPM metadata: %s", pkg, strings.Join(errs, "; "))
	}
	return best, install, nil
}
func latestELRepoKernel(osinfo OSInfo) (RPMCandidate, error) {
	major := strings.SplitN(osinfo.Version, ".", 2)[0]
	if major != "8" && major != "9" && major != "10" {
		return RPMCandidate{}, errors.New("ELRepo is not published for this major version")
	}
	base := fmt.Sprintf("https://www.elrepo.org/linux/kernel/el%s/x86_64", major)
	packages, e := rpmPrimary(base)
	if e != nil {
		return RPMCandidate{}, e
	}
	var best RPMCandidate
	for _, p := range packages {
		if p.Name != "kernel-ml-core" {
			continue
		}
		if best.EVR == "" || rpmvercmp(p.EVR, best.EVR) > 0 {
			best = p
		}
	}
	if best.EVR == "" {
		return best, errors.New("kernel-ml-core not found in ELRepo metadata")
	}
	return best, nil
}
func repoConfigured(name string) bool {
	for _, r := range parseRPMRepos() {
		if r.Name == name {
			return true
		}
	}
	return false
}

func isRPM(id string) bool {
	switch id {
	case "rhel", "rocky", "almalinux", "centos", "ol":
		return true
	}
	return false
}
func runDNF(args ...string) error {
	out, e := command("dnf", args...)
	if out != "" {
		fmt.Print(out)
	}
	return e
}
func rpmInstalledKernels() []string {
	out, _ := command("rpm", "-qa", "--qf", "%{NAME} %{EPOCHNUM}:%{VERSION}-%{RELEASE}.%{ARCH}\n")
	allowed := map[string]bool{"kernel": true, "kernel-core": true, "kernel-modules": true, "kernel-modules-core": true, "kernel-modules-extra": true, "kernel-uek": true, "kernel-uek-core": true, "kernel-uek-modules": true, "kernel-uek-modules-core": true, "kernel-ml": true, "kernel-ml-core": true, "kernel-ml-modules": true, "kernel-lt": true, "kernel-lt-core": true, "kernel-lt-modules": true}
	var a []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && allowed[f[0]] {
			a = append(a, f[0]+"|"+f[1])
		}
	}
	return a
}
func latestInstalledRPMKernel(pkg string) string {
	out, _ := command("rpm", "-q", pkg, "--qf", "%{EPOCHNUM}:%{VERSION}-%{RELEASE}\n")
	best := ""
	for _, v := range strings.Fields(out) {
		if best == "" || rpmvercmp(v, best) > 0 {
			best = v
		}
	}
	return best
}
func runningRPMKernelPackage() (string, string) {
	out, _ := command("rpm", "-qf", "--qf", "%{NAME}|%{EPOCHNUM}:%{VERSION}-%{RELEASE}.%{ARCH}", "/boot/vmlinuz-"+strings.TrimSpace(string(must(os.ReadFile("/proc/sys/kernel/osrelease")))))
	p := strings.SplitN(strings.TrimSpace(out), "|", 2)
	if len(p) != 2 {
		return "", ""
	}
	return p[0], p[1]
}
func runRPMUpdate(candidate RPMCandidate, install string, force bool, repo string) error {
	target := install + "-" + strings.TrimPrefix(candidate.EVR, "0:") + "." + candidate.Arch
	extra := []string{}
	if repo != "" {
		extra = append(extra, "--enablerepo="+repo)
	}

	var sp *spinner
	if !verboseMode {
		sp = startSpinner()
	}

	runDNFQuiet := func(args ...string) error {
		if verboseMode {
			return runDNF(args...)
		}
		out, e := command("dnf", args...)
		if e != nil {
			return fmt.Errorf("%w: %s", e, strings.TrimSpace(out))
		}
		return nil
	}

	// clean metadata
	if verboseMode {
		if out, e := command("dnf", "clean", "metadata"); out != "" {
			fmt.Print(out)
		} else if e != nil {
			if sp != nil {
				sp.Stop()
			}
			return fmt.Errorf("dnf clean metadata failed: %w", e)
		}
	} else {
		out, e := command("dnf", "clean", "metadata")
		if e != nil {
			sp.Stop()
			return fmt.Errorf("dnf clean metadata failed: %w: %s", e, strings.TrimSpace(out))
		}
	}

	dryArgs := append([]string{}, extra...)
	dryArgs = append(dryArgs, "-y", "--setopt=tsflags=test", "install", target)
	if verboseMode {
		fmt.Printf("Installation dry-run: dnf %s\n", strings.Join(dryArgs, " "))
	}
	if e := runDNFQuiet(dryArgs...); e != nil {
		if sp != nil {
			sp.Stop()
		}
		return fmt.Errorf("installation dry-run failed; stopped: %w", e)
	}

	installArgs := append([]string{}, extra...)
	installArgs = append(installArgs, "-y", "install", target)
	if e := runDNFQuiet(installArgs...); e != nil {
		if sp != nil {
			sp.Stop()
		}
		return fmt.Errorf("installation failed: %w", e)
	}

	// Retain the running kernel and the newest installed kernel; preview removal before execution.
	runningName, runningEVR := runningRPMKernelPackage()
	installed := rpmInstalledKernels()
	var remove []string
	targetEVR := strings.TrimPrefix(candidate.EVR, "0:")
	for _, v := range installed {
		p := strings.SplitN(v, "|", 2)
		if len(p) != 2 || p[0] == "" {
			continue
		}
		if p[1] == runningEVR || strings.HasPrefix(strings.TrimPrefix(p[1], "0:"), targetEVR+".") {
			continue
		}
		if p[0] == runningName {
			continue
		}
		nevra := p[0] + "-" + strings.TrimPrefix(p[1], "0:")
		remove = append(remove, nevra)
	}
	if len(remove) > 0 {
		if verboseMode {
			fmt.Printf("Removal dry-run: dnf -y --setopt=tsflags=test remove %s\n", strings.Join(remove, " "))
		}
		args := append([]string{"-y", "--setopt=tsflags=test", "remove"}, remove...)
		if e := runDNFQuiet(args...); e != nil {
			if sp != nil {
				sp.Stop()
			}
			return fmt.Errorf("removal dry-run failed; stopped: %w", e)
		}
		args = append([]string{"-y", "remove"}, remove...)
		if e := runDNFQuiet(args...); e != nil {
			if sp != nil {
				sp.Stop()
			}
			return fmt.Errorf("kernel removal failed: %w", e)
		}
	}

	if verboseMode {
		if err := updateRHELGrub(); err != nil {
			if sp != nil {
				sp.Stop()
			}
			return fmt.Errorf("GRUB update failed: %w", err)
		}
	} else {
		if err := updateRHELGrubQuiet(); err != nil {
			sp.Stop()
			return fmt.Errorf("GRUB update failed: %w", err)
		}
	}

	if sp != nil {
		sp.Stop()
	}

	if force && !chrootMode {
		_, e := command("systemctl", "reboot")
		return e
	}
	return nil
}
func unique(a []string) []string {
	m := map[string]bool{}
	var o []string
	for _, v := range a {
		if !m[v] {
			m[v] = true
			o = append(o, v)
		}
	}
	return o
}
func updateRHELGrubQuiet() error {
	if out, e := command("dnf", "clean", "metadata"); e != nil {
		return fmt.Errorf("dnf clean metadata failed: %w: %s", e, strings.TrimSpace(out))
	}
	p := "/etc/default/grub"
	b, e := os.ReadFile(p)
	if e == nil {
		s := string(b)
		set := func(key, val string) string {
			re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=.*$`)
			line := key + `="` + val + `"`
			if re.MatchString(s) {
				s = re.ReplaceAllString(s, line)
			} else {
				s += "\n" + line
			}
			return s
		}
		s = set("GRUB_ENABLE_BLSCFG", "false")
		s = set("GRUB_DEFAULT", "0")
		if e = os.WriteFile(p, []byte(s), 0644); e != nil {
			return e
		}
	}
	hook := "/etc/kernel/install.d/99-grub-update.install"
	if _, e := os.Stat("/etc/kernel/install.d"); e == nil {
		content := "#!/bin/sh\n[ \"$1\" = add ] || exit 0\nexec grub2-mkconfig -o /boot/grub2/grub.cfg\n"
		if e := os.WriteFile(hook, []byte(content), 0755); e != nil {
			return e
		}
	}
	if _, e := exec.LookPath("grub2-mkconfig"); e == nil {
		out, er := command("grub2-mkconfig", "-o", "/boot/grub2/grub.cfg")
		if er == nil {
			return nil
		}
		if _, st := os.Stat("/boot/efi/EFI/redhat"); st == nil {
			out, er = command("grub2-mkconfig", "-o", "/boot/efi/EFI/redhat/grub.cfg")
			return fmt.Errorf("%w: %s", er, strings.TrimSpace(out))
		}
		return fmt.Errorf("%w: %s", er, strings.TrimSpace(out))
	}
	return errors.New("grub2-mkconfig not found")
}
func updateRHELGrub() error {
	if out, e := command("dnf", "clean", "metadata"); out != "" {
		fmt.Print(out)
	} else if e != nil {
		return e
	}
	// Preserve the requested RHEL boot policy; do not touch Debian BLS configuration.
	p := "/etc/default/grub"
	b, e := os.ReadFile(p)
	if e == nil {
		s := string(b)
		set := func(key, val string) string {
			re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=.*$`)
			line := key + `="` + val + `"`
			if re.MatchString(s) {
				s = re.ReplaceAllString(s, line)
			} else {
				s += "\n" + line
			}
			return s
		}
		s = set("GRUB_ENABLE_BLSCFG", "false")
		s = set("GRUB_DEFAULT", "0")
		if e = os.WriteFile(p, []byte(s), 0644); e != nil {
			return e
		}
	}
	hook := "/etc/kernel/install.d/99-grub-update.install"
	if _, e := os.Stat("/etc/kernel/install.d"); e == nil {
		content := "#!/bin/sh\n[ \"$1\" = add ] || exit 0\nexec grub2-mkconfig -o /boot/grub2/grub.cfg\n"
		if e := os.WriteFile(hook, []byte(content), 0755); e != nil {
			return e
		}
	}
	if _, e := exec.LookPath("grub2-mkconfig"); e == nil {
		out, er := command("grub2-mkconfig", "-o", "/boot/grub2/grub.cfg")
		if out != "" {
			fmt.Print(out)
		}
		if er == nil {
			return nil
		}
		if _, st := os.Stat("/boot/efi/EFI/redhat"); st == nil {
			out, er = command("grub2-mkconfig", "-o", "/boot/efi/EFI/redhat/grub.cfg")
			if out != "" {
				fmt.Print(out)
			}
			return er
		}
		return er
	}
	return errors.New("grub2-mkconfig not found")
}
func isWSL() bool {
	b, _ := os.ReadFile("/proc/version")
	return strings.Contains(strings.ToLower(string(b)), "microsoft")
}
func isVirtualization() string {
	b, _ := os.ReadFile("/sys/class/dmi/id/sys_vendor")
	s := strings.ToLower(string(b))
	for _, v := range []string{"qemu", "kvm", "vmware", "xen", "microsoft", "amazon", "google", "openstack", "proxmox"} {
		if strings.Contains(s, v) {
			return v
		}
	}
	return "unknown"
}

func cleanupDebianKernels(newImage string) error {
	installed := installedKernelPackages()
	running := "linux-image-" + strings.TrimSpace(string(must(os.ReadFile("/proc/sys/kernel/osrelease"))))
	var remove []string
	for _, p := range installed {
		normalized := strings.TrimSuffix(p, ":amd64")
		if normalized == running || normalized == newImage || strings.Contains(newImage, strings.TrimPrefix(normalized, "linux-image-")) {
			continue
		}
		if regexp.MustCompile(`^linux-image-[0-9]`).MatchString(p) {
			remove = append(remove, p)
		}
	}
	sort.Strings(remove)
	remove = unique(remove)
	if len(remove) == 0 {
		return nil
	}
	args := append([]string{"-s", "purge", "-y"}, remove...)
	if verboseMode {
		fmt.Printf("Removal dry-run: apt-get %s\n", strings.Join(args, " "))
	}
	if verboseMode {
		if e := runApt(args...); e != nil {
			return fmt.Errorf("removal dry-run failed; stopped: %w", e)
		}
	} else {
		if out, e := command("apt-get", args...); e != nil {
			return fmt.Errorf("removal dry-run failed; stopped: %w: %s", e, strings.TrimSpace(out))
		}
	}
	args = append([]string{"purge", "-y"}, remove...)
	if verboseMode {
		return runApt(args...)
	}
	out, e := command("apt-get", args...)
	if e != nil {
		return fmt.Errorf("%w: %s", e, strings.TrimSpace(out))
	}
	return nil
}
// prepareOfficialRPMRepos temporarily replaces enabled RPM repositories with
// the official ones for the detected distribution, returns a restore function.
func prepareOfficialRPMRepos(id, version string) (func(), error) {
	files, _ := filepath.Glob("/etc/yum.repos.d/*.repo")
	backupDir := "/tmp/kernel_update_repo_backup"
	os.RemoveAll(backupDir)
	if e := os.MkdirAll(backupDir, 0755); e != nil {
		return nil, e
	}
	var moved []string
	for _, f := range files {
		base := filepath.Base(f)
		if e := os.Rename(f, filepath.Join(backupDir, base)); e != nil {
			for _, m := range moved {
				os.Rename(filepath.Join(backupDir, filepath.Base(m)), m)
			}
			return nil, e
		}
		moved = append(moved, f)
	}
	restore := func() {
		entries, _ := os.ReadDir("/etc/yum.repos.d")
		for _, e := range entries {
			if e.Name() == "99-kernel_update-official.repo" {
				os.Remove(filepath.Join("/etc/yum.repos.d", e.Name()))
			}
		}
		for _, m := range moved {
			os.Rename(filepath.Join(backupDir, filepath.Base(m)), m)
		}
		os.RemoveAll(backupDir)
	}
	repoFile := "/etc/yum.repos.d/99-kernel_update-official.repo"
	var content string
	major := strings.SplitN(version, ".", 2)[0]

	switch id {
	case "centos":
		switch major {
		case "10":
			content = `[official-baseos]
name=Official CentOS Stream 10 BaseOS
baseurl=https://mirror.stream.centos.org/10-stream/BaseOS/x86_64/os/
enabled=1
gpgcheck=0

[official-appstream]
name=Official CentOS Stream 10 AppStream
baseurl=https://mirror.stream.centos.org/10-stream/AppStream/x86_64/os/
enabled=1
gpgcheck=0
`
		case "9":
			content = `[official-baseos]
name=Official CentOS Stream 9 BaseOS
baseurl=https://mirror.stream.centos.org/9-stream/BaseOS/x86_64/os/
enabled=1
gpgcheck=0

[official-appstream]
name=Official CentOS Stream 9 AppStream
baseurl=https://mirror.stream.centos.org/9-stream/AppStream/x86_64/os/
enabled=1
gpgcheck=0
`
		case "8":
			content = `[official-baseos]
name=CentOS Stream 8 BaseOS (Vault)
baseurl=https://vault.centos.org/8-stream/BaseOS/x86_64/os/
enabled=1
gpgcheck=0

[official-appstream]
name=CentOS Stream 8 AppStream (Vault)
baseurl=https://vault.centos.org/8-stream/AppStream/x86_64/os/
enabled=1
gpgcheck=0
`
		case "7":
			content = `[official-baseos]
name=CentOS 7 Base (Vault)
baseurl=https://vault.centos.org/7.9.2009/os/x86_64/
enabled=1
gpgcheck=0

[official-updates]
name=CentOS 7 Updates (Vault)
baseurl=https://vault.centos.org/7.9.2009/updates/x86_64/
enabled=1
gpgcheck=0
`
		case "6":
			content = `[official-baseos]
name=CentOS 6 Base (Vault)
baseurl=https://vault.centos.org/6.10/os/x86_64/
enabled=1
gpgcheck=0

[official-updates]
name=CentOS 6 Updates (Vault)
baseurl=https://vault.centos.org/6.10/updates/x86_64/
enabled=1
gpgcheck=0
`
		default:
			restore()
			return nil, fmt.Errorf("no official CentOS repository template for major version %s", major)
		}

	case "rocky":
		content = fmt.Sprintf(`[official-baseos]
name=Official Rocky BaseOS
baseurl=https://dl.rockylinux.org/pub/rocky/%s/BaseOS/x86_64/os/
enabled=1
gpgcheck=0

[official-appstream]
name=Official Rocky AppStream
baseurl=https://dl.rockylinux.org/pub/rocky/%s/AppStream/x86_64/os/
enabled=1
gpgcheck=0
`, major, major)

	case "almalinux":
		content = fmt.Sprintf(`[official-baseos]
name=Official AlmaLinux BaseOS
baseurl=https://repo.almalinux.org/almalinux/%s/BaseOS/x86_64/os/
enabled=1
gpgcheck=0

[official-appstream]
name=Official AlmaLinux AppStream
baseurl=https://repo.almalinux.org/almalinux/%s/AppStream/x86_64/os/
enabled=1
gpgcheck=0
`, major, major)

	case "rhel":
		switch major {
		case "10":
			content = `[official-baseos]
name=Official RHEL 10 BaseOS
baseurl=https://cdn.redhat.com/content/dist/rhel10/10/x86_64/baseos/os
enabled=1
gpgcheck=0

[official-appstream]
name=Official RHEL 10 AppStream
baseurl=https://cdn.redhat.com/content/dist/rhel10/10/x86_64/appstream/os
enabled=1
gpgcheck=0
`
		case "9":
			content = `[official-baseos]
name=Official RHEL 9 BaseOS
baseurl=https://cdn.redhat.com/content/dist/rhel9/9/x86_64/baseos/os
enabled=1
gpgcheck=0

[official-appstream]
name=Official RHEL 9 AppStream
baseurl=https://cdn.redhat.com/content/dist/rhel9/9/x86_64/appstream/os
enabled=1
gpgcheck=0
`
		case "8":
			content = `[official-baseos]
name=Official RHEL 8 BaseOS
baseurl=https://cdn.redhat.com/content/dist/rhel8/8/x86_64/baseos/os
enabled=1
gpgcheck=0

[official-appstream]
name=Official RHEL 8 AppStream
baseurl=https://cdn.redhat.com/content/dist/rhel8/8/x86_64/appstream/os
enabled=1
gpgcheck=0
`
		case "7":
			content = `[official-baseos]
name=Official RHEL 7 Server
baseurl=https://cdn.redhat.com/content/dist/rhel/server/7/7Server/x86_64/os
enabled=1
gpgcheck=0
`
		case "6":
			content = `[official-baseos]
name=Official RHEL 6 Server
baseurl=https://cdn.redhat.com/content/dist/rhel/server/6/6Server/x86_64/os
enabled=1
gpgcheck=0
`
		default:
			restore()
			return nil, fmt.Errorf("no official RHEL repository template for major version %s", major)
		}

	case "ol":
		content = fmt.Sprintf(`[official-baseos]
name=Official Oracle Linux BaseOS
baseurl=https://yum.oracle.com/repo/OracleLinux/OL%s/baseos/latest/x86_64/
enabled=1
gpgcheck=0

[official-appstream]
name=Official Oracle Linux AppStream
baseurl=https://yum.oracle.com/repo/OracleLinux/OL%s/appstream/latest/x86_64/
enabled=1
gpgcheck=0
`, major, major)

	default:
		restore()
		return nil, fmt.Errorf("no official RPM repository template for %s", id)
	}

	if e := os.WriteFile(repoFile, []byte(content), 0644); e != nil {
		restore()
		return nil, e
	}
	return restore, nil
}

func main() {
	// The standard flag package stops at the first positional argument; move the mode last
	// so both `kernel_update --force-reboot update` and `kernel_update update --force-reboot` work.
	args := os.Args[1:]
	var positional []string
	var options []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "check" || a == "update" {
			positional = append(positional, a)
			continue
		}
		options = append(options, a)
		if (a == "--proxy" || a == "-proxy") && i+1 < len(args) {
			i++
			options = append(options, args[i])
		}
	}
	os.Args = append([]string{os.Args[0]}, append(options, positional...)...)

	// version flag handled early
	proxy := flag.String("proxy", "", "HTTP/HTTPS/SOCKS5 proxy URL")
	force := flag.Bool("force-reboot", false, "reboot after successful update")
	verbose := flag.Bool("verbose", false, "enable extended logging")
	showVersion := flag.Bool("v", false, "show version")
	showVersionLong := flag.Bool("version", false, "show version")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [check|update] [options]\n", filepath.Base(os.Args[0]))
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion || *showVersionLong {
		fmt.Printf("kernel_update %s\n", appVersion)
		return
	}
	if flag.NArg() > 0 {
		pos := flag.Arg(0)
		if pos != "check" && pos != "update" {
			fmt.Fprintf(os.Stderr, "usage: kernel_update [check|update] [--proxy URL] [--force-reboot] [--verbose] [-v|--version]\n")
			os.Exit(2)
		}
	}
	verboseMode = *verbose
	// Show logo only in interactive check/update mode.
	showLogo()

	oi, e := readOS()
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if runtime.GOARCH != "amd64" {
		fmt.Printf("Unsupported architecture: %s (only x86_64)\n", runtime.GOARCH)
		os.Exit(1)
	}
	client, e = httpClient(*proxy)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}

	// Check end-of-life status after the HTTP client is initialized.
	if eol, date, err := checkEOL(oi); err == nil && eol {
		printError(fmt.Sprintf("this OS (%s %s) reached end-of-life on %s.", oi.ID, oi.Version, date))
		fmt.Println()
		fmt.Println("No official kernel update available.")
		if isRPM(oi.ID) {
			if cand, _, er := latestRPMKernel(oi); er == nil && cand.EVR != "" {
				fmt.Printf("Latest kernel version was - %s.\n", strings.TrimPrefix(cand.EVR, "0:"))
			}
		} else if oi.ID == "debian" || oi.ID == "ubuntu" {
			if _, image, er := latestDistroKernel(oi); er == nil && image != "" {
				fmt.Printf("Latest kernel version was - %s.\n", strings.TrimPrefix(image, "linux-image-"))
			}
		}
		os.Exit(1)
	}

	runningKernel := strings.TrimSpace(string(must(os.ReadFile("/proc/sys/kernel/osrelease"))))
	chrootMode = isChroot()
	current := runningKernel
	if chrootMode {
		current = "unknown (chroot; running Rescue kernel: " + runningKernel + ")"
	}
	if isWSL() {
		fmt.Println("WSL detected: update the kernel using the Windows/WSL update mechanism.")
		return
	}
	if isContainer() {
		fmt.Println("Container detected: the kernel must be updated on the parent host.")
		return
	}
	virt := isVirtualization()

	if isRPM(oi.ID) {
		major, _ := strconv.Atoi(strings.SplitN(oi.Version, ".", 2)[0])
		if major < 8 {
			printError(fmt.Sprintf("Unsupported RPM distribution version: %s %s (DNF-based EL 8+ is required).", oi.ID, oi.Version))
			os.Exit(1)
		}

		var sp *spinner
		if !verboseMode {
			sp = startSpinner()
		}
		cand, install, er := latestRPMKernel(oi)
		pkg := "kernel-core"
		if oi.ID == "ol" {
			pkg = "kernel-uek-core"
		}
		currentEVR := ""
		installedEVR := ""
		if er == nil {
			if chrootMode {
				currentEVR = latestInstalledRPMKernel(pkg)
				installedEVR = currentEVR
			} else {
				_, currentEVR = runningRPMKernelPackage()
				installedEVR = latestInstalledRPMKernel(pkg)
			}
		}
		elrepo, elrepoErr := latestELRepoKernel(oi)
		if sp != nil {
			sp.Stop()
		}
		if er != nil {
			printError(fmt.Sprintf("Recommended distribution kernel: unavailable: %v", er))
			os.Exit(1)
		}
		if currentEVR == "" {
			printError("Cannot determine installed kernel package version.")
			os.Exit(1)
		}
		updateAvailable := rpmvercmp(cand.EVR, currentEVR) > 0
		elrepoNewer := elrepoErr == nil && rpmvercmp(elrepo.EVR, cand.EVR) > 0

		if verboseMode {
			fmt.Printf("OS: %s %s (%s)\nArchitecture: x86_64\nCurrent kernel: %s\n", oi.ID, oi.Version, oi.Codename, current)
			fmt.Printf("Virtualization: %s\n", virt)
			fmt.Printf("Latest official kernel: %s\nRecommended package: %s (%s)\nRepository: %s\n", cand.EVR, install, cand.Name, cand.Repo)
			fmt.Printf("Installed kernel package version: %s\n", currentEVR)
			fmt.Printf("Update available: %s\n", map[bool]string{true: "yes", false: "no"}[updateAvailable])
			if elrepoNewer {
				fmt.Printf("Optional third-party kernel (ELRepo kernel-ml): %s\n", elrepo.EVR)
			}
		}

		if !updateAvailable && !elrepoNewer {
			// Running kernel equals latest available: nothing to do.
			fmt.Printf("Running kernel %s is the %slatest available%s for this OS.\n", strings.TrimPrefix(currentEVR, "0:"), green, reset)
			return
		}

		// Latest official kernel is already installed, but a reboot has not been performed yet.
		if !chrootMode && installedEVR != "" && rpmvercmp(installedEVR, currentEVR) > 0 && rpmvercmp(cand.EVR, installedEVR) <= 0 && !elrepoNewer {
			fmt.Printf("Running kernel - %s.\nInstalled kernel - %s is the latest available.\n%sReboot OS to use it.%s\n",
				strings.TrimPrefix(currentEVR, "0:"), strings.TrimPrefix(installedEVR, "0:"), green, reset)
			return
		}

		installRPM := func(useELRepo bool) error {
			restore, er := prepareOfficialRPMRepos(oi.ID, oi.Version)
			if er != nil {
				return fmt.Errorf("failed to prepare official repositories: %w", er)
			}
			defer restore()
			if useELRepo {
				return runRPMUpdate(elrepo, "kernel-ml", *force, "elrepo-kernel")
			}
			return runRPMUpdate(cand, install, *force, "")
		}

		if elrepoNewer && repoConfigured("elrepo-kernel") {
			if confirmYesNo(fmt.Sprintf("Install the newer third-party ELRepo kernel %s instead of the distribution kernel? [y/N]: ", elrepo.EVR)) {
				if e = installRPM(true); e != nil {
					printFailed(strings.TrimPrefix(elrepo.EVR, "0:"), e)
					os.Exit(1)
				}
				printSuccess(strings.TrimPrefix(elrepo.EVR, "0:"))
				printReboot()
				return
			}
		}
		if !updateAvailable {
			// No newer official kernel; user declined the newer ELRepo kernel.
			return
		}
		if !confirmYesNo("Install the latest kernel available ? [y/N]: ") {
			return
		}
		if e = installRPM(false); e != nil {
			printFailed(strings.TrimPrefix(cand.EVR, "0:"), e)
			os.Exit(1)
		}
		printSuccess(strings.TrimPrefix(cand.EVR, "0:"))
		if chrootMode {
			fmt.Println("Exit chroot and reboot the host manually to start the installed kernel.")
		} else {
			printReboot()
		}
		return
	}

	if oi.ID == "debian" || oi.ID == "ubuntu" {
		var sp *spinner
		if !verboseMode {
			sp = startSpinner()
		}
		meta, image, er := latestDistroKernel(oi)
		imageVersion := strings.TrimPrefix(image, "linux-image-")
		currentVersion := current
		installedVersion := ""
		if er == nil {
			if chrootMode {
				currentVersion = latestInstalledDebianKernel()
				installedVersion = currentVersion
			} else {
				installedVersion = latestInstalledDebianKernel()
			}
		}
		if sp != nil {
			sp.Stop()
		}
		if er != nil {
			printError(fmt.Sprintf("Recommended distribution kernel: unavailable: %v", er))
			os.Exit(1)
		}
		if currentVersion == "" {
			printError("Cannot determine installed kernel package version.")
			os.Exit(1)
		}
		updateAvailable := strings.HasPrefix(image, "linux-image-") && debVersionGreater(imageVersion, currentVersion)

		if verboseMode {
			fmt.Printf("OS: %s %s (%s)\nArchitecture: x86_64\nCurrent kernel: %s\n", oi.ID, oi.Version, oi.Codename, current)
			fmt.Printf("Virtualization: %s\n", virt)
			fmt.Printf("Latest official meta-package version: %s\nRecommended kernel package: %s\n", meta, image)
			fmt.Printf("Installed kernel package version: %s\n", currentVersion)
			fmt.Printf("Update available: %s\n", map[bool]string{true: "yes", false: "no"}[updateAvailable])
		}

		if !updateAvailable {
			fmt.Printf("Running kernel %s is the %slatest available%s for this OS.\n", currentVersion, green, reset)
			return
		}

		// Latest official kernel is already installed, but a reboot has not been performed yet.
		if !chrootMode && installedVersion != "" && debVersionGreater(installedVersion, currentVersion) && !debVersionGreater(imageVersion, installedVersion) {
			fmt.Printf("Running kernel - %s.\nInstalled kernel - %s is the latest available.\n%sReboot OS to use it.%s\n",
				currentVersion, installedVersion, green, reset)
			return
		}

		if !confirmYesNo("Install the latest kernel available ? [y/N]: ") {
			return
		}

		installApt := func() error {
			type aptRestore struct{ path, content string }
			var restores []aptRestore
			sourcesFiles, _ := filepath.Glob("/etc/apt/sources.list.d/*.sources")
			listFiles, _ := filepath.Glob("/etc/apt/sources.list.d/*.list")
			listFiles = append(listFiles, "/etc/apt/sources.list")
			allFiles := append(sourcesFiles, listFiles...)
			for _, f := range allFiles {
				b, err := os.ReadFile(f)
				if err != nil {
					continue
				}
				restores = append(restores, aptRestore{f, string(b)})
			}
			backupDir := "/tmp/kernel_update_apt_backup"
			os.RemoveAll(backupDir)
			os.MkdirAll(backupDir, 0755)
			for _, r := range restores {
				os.Rename(r.path, filepath.Join(backupDir, filepath.Base(r.path)))
			}
			officialDir := "/etc/apt/sources.list.d"
			os.MkdirAll(officialDir, 0755)
			if oi.ID == "debian" {
				content := fmt.Sprintf("deb http://deb.debian.org/debian %s main\ndeb http://security.debian.org/debian-security %s-security main\n", oi.Codename, oi.Codename)
				os.WriteFile(filepath.Join(officialDir, "99-kernel_update-official.list"), []byte(content), 0644)
			} else if oi.ID == "ubuntu" {
				content := fmt.Sprintf("deb http://archive.ubuntu.com/ubuntu %s main\ndeb http://security.ubuntu.com/ubuntu %s-security main\n", oi.Codename, oi.Codename)
				os.WriteFile(filepath.Join(officialDir, "99-kernel_update-official.list"), []byte(content), 0644)
			}
			// Refresh package indexes from the temporary official sources.
			if verboseMode {
				fmt.Println("Refreshing package indexes from official sources: apt-get update")
			}
			if _, err := command("apt-get", "update"); err != nil && !verboseMode {
				// apt-get update may return non-zero on some warnings; surface the output for debugging.
				fmt.Println(err.Error())
			}
			restoreApt := func() {
				os.Remove(filepath.Join(officialDir, "99-kernel_update-official.list"))
				os.Remove(filepath.Join(officialDir, "99-kernel_update-official-security.list"))
				for _, r := range restores {
					os.Rename(filepath.Join(backupDir, filepath.Base(r.path)), r.path)
				}
				os.RemoveAll(backupDir)
			}
			defer restoreApt()

			var sp *spinner
			if !verboseMode {
				sp = startSpinner()
			}
			runQuiet := func(args ...string) error {
				if verboseMode {
					return runApt(args...)
				}
				out, e := command("apt-get", args...)
				if e != nil {
					return fmt.Errorf("%w: %s", e, strings.TrimSpace(out))
				}
				return nil
			}
			if e := runQuiet("-s", "install", "-y", image); e != nil {
				if sp != nil {
					sp.Stop()
				}
				return fmt.Errorf("installation dry-run failed; stopped: %w", e)
			}
			if e := runQuiet("install", "-y", image); e != nil {
				if sp != nil {
					sp.Stop()
				}
				return fmt.Errorf("installation failed: %w", e)
			}
			if e := cleanupDebianKernels(image); e != nil {
				if sp != nil {
					sp.Stop()
				}
				return fmt.Errorf("kernel cleanup stopped: %w", e)
			}
			if sp != nil {
				sp.Stop()
			}
			return nil
		}

		if e = installApt(); e != nil {
			printFailed(imageVersion, e)
			os.Exit(1)
		}
		printSuccess(imageVersion)
		if chrootMode {
			fmt.Println("Exit chroot and reboot the host manually to start the installed kernel.")
		} else {
			printReboot()
		}
		if *force {
			_, e = command("systemctl", "reboot")
			if e != nil {
				printError(e.Error())
				os.Exit(1)
			}
		}
		return
	}
	printError(fmt.Sprintf("Unsupported distribution: %s. No update attempted.", oi.ID))
	os.Exit(1)
}

func must(b []byte, e error) []byte {
	if e != nil {
		return []byte("unknown")
	}
	return b
}