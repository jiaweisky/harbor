#!/usr/bin/env bash
# Fake stream-json CLI: ask two questions in one AskUserQuestion can_use_tool
# request, record the control_response the client sends back (FAKE_QA_LOG), then
# emit the terminal result.
read -r _ # initial stream-json user message
echo '{"type":"control_request","request_id":"perm_qm","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Favorite color?","header":"Color","options":[{"label":"Red"},{"label":"Blue"}]},{"question":"Favorite animal?","header":"Animal","options":[{"label":"Cat"},{"label":"Dog"}]}]},"tool_use_id":"t1"}}'
while read -r line; do
  case "$line" in
    *control_response*)
      printf '%s\n' "$line" >>"$FAKE_QA_LOG"
      break
      ;;
  esac
done
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"answered"}]}}'
echo '{"type":"result","result":"answered","is_error":false}'
