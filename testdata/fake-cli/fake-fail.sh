#!/usr/bin/env bash
# Fake failing CLI: writes to stderr and exits non-zero.
echo "boom: something went wrong" 1>&2
exit 3
