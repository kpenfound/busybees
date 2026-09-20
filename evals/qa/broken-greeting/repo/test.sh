#!/bin/sh
# The test suite: one case.
got=$(sh greet.sh Ada)
if [ "$got" != "Hello, Ada!" ]; then
	echo "greet.sh Ada printed '$got', want 'Hello, Ada!'"
	exit 1
fi
