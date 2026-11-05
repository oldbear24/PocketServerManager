#!/bin/sh
# The entrypoint of PocketCloud's PHP runtime images. At every start it writes
# what cannot be part of the image to /run/pocketcloud-php, then runs the
# command, php-fpm unless another one is given:
#
# - fpm-status.conf: FPM's status page and ping for pocketcloud-php-status and
#   the health check, on a Unix socket of their own (pm.status_listen). FPM
#   answers them on the pool's port 9000 too, which other containers reach, so
#   their paths are made up anew at every start and only exist in here.
# - msmtprc: the SMTP relay for mail(), from POCKETCLOUD_SMTP_HOST, _PORT,
#   _TLS (starttls, tls or off), _USER, _PASSWORD or _PASSWORD_FILE and _FROM.
#   A password goes into a file of its own that msmtp reads, never onto a
#   command line, and POCKETCLOUD_SMTP_PASSWORD is unset before PHP starts.
#
# The container runs as www-data. As root, which only jobs need, nothing is
# written: FPM would not run as root anyway.
set -eu

runtime=/run/pocketcloud-php

warn() {
	echo "pocketcloud-php: $*" >&2
}

newline='
'
carriage_return=$(printf '\r')

# safe_value tells whether a value can stand in msmtp's configuration, which
# has one setting per line and quotes values with double quotes.
safe_value() {
	case $1 in
		*[\"\\]* | *"$newline"* | *"$carriage_return"*) return 1 ;;
	esac
	return 0
}

write_status_config() {
	token=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
	if [ ${#token} -ne 32 ]; then
		warn "cannot make up the status paths, so FPM's status is off"
		return 0
	fi

	rm -f "$runtime/status.sock"
	cat >"$runtime/fpm-status.conf.new" <<-EOF
		; Written by pocketcloud-php-entrypoint at start.
		[www]
		pm.status_listen = $runtime/status.sock
		pm.status_path = /status-$token
		ping.path = /ping-$token
		ping.response = pong
	EOF
	mv -f "$runtime/fpm-status.conf.new" "$runtime/fpm-status.conf"
}

write_mail_config() {
	rm -f "$runtime/msmtprc" "$runtime/smtp-password"

	host=${POCKETCLOUD_SMTP_HOST:-}
	if [ -z "$host" ]; then
		return 0
	fi

	port=${POCKETCLOUD_SMTP_PORT:-587}
	tls=${POCKETCLOUD_SMTP_TLS:-}
	if [ -z "$tls" ]; then
		if [ "$port" = 465 ]; then tls=tls; else tls=starttls; fi
	fi
	user=${POCKETCLOUD_SMTP_USER:-}
	from=${POCKETCLOUD_SMTP_FROM:-}
	password_file=${POCKETCLOUD_SMTP_PASSWORD_FILE:-}

	case $host in
		*[!A-Za-z0-9.:-]*)
			warn "POCKETCLOUD_SMTP_HOST is not a host name or address, so mail is off"
			return 0
			;;
	esac
	case $port in
		'' | *[!0-9]*)
			warn "POCKETCLOUD_SMTP_PORT is not a port, so mail is off"
			return 0
			;;
	esac
	case $tls in
		starttls | tls | off) ;;
		*)
			warn "POCKETCLOUD_SMTP_TLS must be starttls, tls or off, so mail is off"
			return 0
			;;
	esac
	if ! safe_value "$user" || ! safe_value "$from"; then
		warn "POCKETCLOUD_SMTP_USER or _FROM holds a quote, a backslash or a line break, so mail is off"
		return 0
	fi
	case $password_file in
		*[!A-Za-z0-9._/-]*)
			warn "POCKETCLOUD_SMTP_PASSWORD_FILE is not a plain path, so mail is off"
			return 0
			;;
	esac

	{
		echo "# Written by pocketcloud-php-entrypoint at start, from POCKETCLOUD_SMTP_*."
		echo "defaults"
		echo "syslog off"
		# one line per mail on the container's log: the base of the counts of sent and failed mail
		echo "logfile /dev/stderr"
		echo
		echo "account default"
		echo "host $host"
		echo "port $port"
		case $tls in
			starttls) printf 'tls on\ntls_starttls on\n' ;;
			tls) printf 'tls on\ntls_starttls off\n' ;;
			off) echo "tls off" ;;
		esac
		if [ -n "$user" ]; then
			echo "auth on"
			echo "user \"$user\""
			if [ -n "$password_file" ]; then
				echo "passwordeval \"cat $password_file\""
			else
				echo "passwordeval \"cat $runtime/smtp-password\""
			fi
		else
			echo "auth off"
		fi
		# the envelope sender: the one given, or else the From header of the mail
		if [ -n "$from" ]; then
			echo "from \"$from\""
		else
			echo "from %F"
		fi
	} >"$runtime/msmtprc.new"

	if [ -n "$user" ] && [ -z "$password_file" ]; then
		# printf is built into the shell: the password is on no command line
		printf '%s' "${POCKETCLOUD_SMTP_PASSWORD:-}" >"$runtime/smtp-password"
	fi
	mv -f "$runtime/msmtprc.new" "$runtime/msmtprc"
}

if [ "$(id -u)" != 0 ]; then
	if [ -d "$runtime" ] && [ -w "$runtime" ]; then
		umask 077
		write_status_config
		write_mail_config
		umask 022
	else
		warn "$runtime is not writable by this user, so FPM's status and mail are off"
	fi
fi
unset POCKETCLOUD_SMTP_PASSWORD

# like the official image: options alone are options of php-fpm
if [ "$#" -eq 0 ] || [ "${1#-}" != "$1" ]; then
	set -- php-fpm "$@"
fi

exec "$@"
