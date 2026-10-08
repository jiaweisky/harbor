#!/usr/bin/env bash
# Fake stream-json CLI: ask for tool permission via a control_request, wait for
# the control_response, then emit the terminal result.
read -r _ # initial stream-json user message
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo hi"}}]}}'
echo '{"type":"control_request","request_id":"perm_test_1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"echo hi"},"tool_use_id":"toolu_1"}}'
while read -r line; do
  case "$line" in
    *control_response*) break ;;
  esac
done
echo '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"hi","is_error":false}]}}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"allowed"}]}}'
echo '{"type":"result","result":"allowed","is_error":false}'
