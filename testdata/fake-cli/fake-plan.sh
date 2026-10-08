#!/usr/bin/env bash
# Fake stream-json CLI: request plan approval via ExitPlanMode (can_use_tool),
# log the control_response, then emit the terminal result.
read -r _ # initial stream-json user message
echo '{"type":"control_request","request_id":"plan_1","request":{"subtype":"can_use_tool","tool_name":"ExitPlanMode","input":{"plan":"Create README.md"}}}'
while read -r line; do
  case "$line" in
    *control_response*)
      echo "$line" >> "${HARBOR_STDIN_LOG:-/dev/null}"
      break
      ;;
  esac
done
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"approved"}]}}'
echo '{"type":"result","result":"approved","is_error":false}'
