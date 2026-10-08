#!/usr/bin/env bash
# Fake stream-json CLI: emit a schema-validated structured result.
read -r _ # initial stream-json user message
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Paris, 2.1M"}]}}'
echo '{"type":"result","result":"Paris, 2.1M","is_error":false,"structured_output":{"city":"Paris","population_millions":2.1}}'
