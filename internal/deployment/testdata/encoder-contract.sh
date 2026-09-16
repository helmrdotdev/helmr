#!/bin/sh
if [ "$1" = "-version" ]; then
	printf 'mksquashfs version 4.6.1 (2023/03/25)\n'
	exit 0
fi
if [ "$*" != "- /proc/self/fd/3 -tar -noappend -all-root -no-xattrs -no-exports -no-fragments -no-tailends -no-duplicates -no-hardlinks -no-progress -exit-on-error -processors 2 -mem 1024M -comp zstd -b 131072 -root-mode 0755 -mkfs-time 0 -all-time 0" ]; then
	printf 'arguments: %s\n' "$*" >&2
	exit 2
fi
if [ "$LC_ALL" != "C" ] || [ "$TZ" != "UTC" ] ||
	[ -n "${HOME+x}" ] || [ -n "${SOURCE_DATE_EPOCH+x}" ]; then
	printf 'unexpected environment: LC_ALL=%s TZ=%s HOME=%s SOURCE_DATE_EPOCH=%s\n' \
		"$LC_ALL" "$TZ" "${HOME+x}" "${SOURCE_DATE_EPOCH+x}" >&2
	exit 3
fi
printf 'encoded' >&3
