#!/usr/bin/env bash
# Fake exec CLI: write a PNG into the working directory (a stand-in for a
# text-to-image result), then print a short result.
printf '\x89PNG\r\n\x1a\nfake' >"$PWD/generated.png"
echo "created generated.png"
