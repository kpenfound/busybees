#!/bin/sh
# Print a greeting for the name given.
if [ $# -eq 0 ]; then
	echo "usage: greet.sh <name>"
	exit 1
fi
echo "Hello, $1!"
