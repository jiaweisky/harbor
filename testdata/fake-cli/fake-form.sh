#!/usr/bin/env bash
# Fake stream-json CLI: ask a form question via an elicitation_create control
# request, log the control_response, then emit the terminal result.
read -r _ # initial stream-json user message
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserForStructuredInput","input":{"message":"Pick a color"}}]}}'
echo '{"type":"control_request","request_id":"el_1","request":{"subtype":"elicitation_create","mode":"form","message":"Pick a color","requestedSchema":{"type":"object","properties":{"color":{"type":"string","enum":["red","blue"]}},"required":["color"]}}}'
while read -r line; do
  echo "$line" >> "${HARBOR_STDIN_LOG:-/dev/null}"
  case "$line" in
    *control_response*) break ;;
  esac
done
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"chose blue"}]}}'
echo '{"type":"result","result":"chose blue","is_error":false}'
