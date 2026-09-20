#!/bin/sh
# The test suite: one case per documented behaviour.
got=$(sh greet.sh Ada)
if [ "$got" != "Hello, Ada!" ]; then
	echo "greet.sh Ada printed '$got', want 'Hello, Ada!'"
	exit 1
fi
got=$(sh greet.sh)
if [ "$got" != "usage: greet.sh <name>" ]; then
	echo "greet.sh with no name printed '$got', want the usage line"
	exit 1
fi
echo ok
