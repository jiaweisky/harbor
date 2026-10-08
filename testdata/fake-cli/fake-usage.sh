#!/usr/bin/env bash
# Fake stream-json CLI: emit a terminal result carrying token/cost usage.
read -r _ # initial stream-json user message
echo '{"type":"result","result":"ok","is_error":false,"total_cost_usd":0.0123,"usage":{"input_tokens":11,"output_tokens":22}}'
