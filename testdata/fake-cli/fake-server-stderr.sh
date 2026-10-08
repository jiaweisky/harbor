#!/usr/bin/env bash
# Fake server that reports its listening address on stderr (as codebuddy's
# streamable-http transport does), then stays up.
echo "endpoint: http://127.0.0.1:12345/api/v1/acp" 1>&2
sleep 30
