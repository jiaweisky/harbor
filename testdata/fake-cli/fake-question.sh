#!/usr/bin/env bash
# Fake stream-json CLI: ask a question via an AskUserQuestion can_use_tool
# control_request, wait for the answer, then emit the terminal result.
read -r _ # initial stream-json user message
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"AskUserQuestion","input":{"questions":[{"question":"Favorite color?","header":"Color","multiSelect":false,"options":[{"label":"Red","description":""},{"label":"Blue","description":""}]}]}}]}}'
echo '{"type":"control_request","request_id":"perm_q1","request":{"subtype":"can_use_tool","tool_name":"AskUserQuestion","input":{"questions":[{"question":"Favorite color?","header":"Color","multiSelect":false,"options":[{"label":"Red","description":""},{"label":"Blue","description":""}]}]},"tool_use_id":"t1"}}'
while read -r line; do
  case "$line" in
    *control_response*) break ;;
  esac
done
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"chose Blue"}]}}'
echo '{"type":"result","result":"chose Blue","is_error":false}'
