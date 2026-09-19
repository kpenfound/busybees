#!/bin/sh
out=$(sh greet.sh Ada)
if [ "$out" != "Hello, Ada!" ]; then
	echo "sh greet.sh Ada printed: $out" >&2
	exit 1
fi
