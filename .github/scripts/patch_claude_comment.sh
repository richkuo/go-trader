#!/usr/bin/env bash
set -euo pipefail

BOT_LOGIN="${BOT_LOGIN:-claude[bot]}"
RUN_ID="${RUN_ID:-}"
ON_MISS="${ON_MISS:-skip}"
TARGET_COMMENT_ID="${TARGET_COMMENT_ID:-}"
SELECT_ONLY="${SELECT_ONLY:-}"

if [ -n "$TARGET_COMMENT_ID" ]; then
  COMMENT=$(gh api "repos/${REPO}/issues/comments/${TARGET_COMMENT_ID}" 2>/dev/null || printf 'null')
else
  COMMENT=$(gh api --paginate --slurp "repos/${REPO}/issues/${ISSUE_NUMBER}/comments" \
    | jq --arg bot "$BOT_LOGIN" --arg run "$RUN_ID" \
        '[.[][]
          | select(.user.login == $bot)
          | select($run == "" or (.body | test("/actions/runs/" + $run + "([^0-9]|$)")))]
         | sort_by(.updated_at) | last')
fi

COMMENT_ID=$(printf '%s' "$COMMENT" | jq -r '.id')

if [ -n "$SELECT_ONLY" ]; then
  if [ -n "$COMMENT_ID" ] && [ "$COMMENT_ID" != "null" ]; then
    printf '%s' "$COMMENT_ID"
  fi
  exit 0
fi

if [ -z "$COMMENT_ID" ] || [ "$COMMENT_ID" = "null" ]; then
  if [ "$ON_MISS" = "post" ] && [ -n "${STATUS_NOTE:-}" ]; then
    NEW_BODY=$(BODY_IN="" python3 .github/scripts/compose_claude_comment.py)
    gh api "repos/${REPO}/issues/${ISSUE_NUMBER}/comments" \
      --method POST \
      --field body="$NEW_BODY"
    exit 0
  fi
  echo "No ${BOT_LOGIN} comment found — nothing to update."
  exit 0
fi

BODY=$(printf '%s' "$COMMENT" | jq -r '.body')

NEW_BODY=$(BODY_IN="$BODY" python3 .github/scripts/compose_claude_comment.py)

gh api "repos/${REPO}/issues/comments/${COMMENT_ID}" \
  --method PATCH \
  --field body="$NEW_BODY"
