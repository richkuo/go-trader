#!/usr/bin/env bash
# Patch the latest Claude-authored comment on an issue/PR with the
# authoritative LLM footer (and an optional workflow status note).
#
# Env: REPO, ISSUE_NUMBER, GH_TOKEN, MODEL_ID, EFFORT, CLAUDE_HARNESS,
#      STATUS_NOTE (optional) — the last four are consumed by
#      compose_claude_comment.py.
#      BOT_LOGIN (optional, default claude[bot]) — which author's comment to
#      patch. The least-privilege review job (#1178) binds the agent to the
#      job token, so its comments post as github-actions[bot].
#
# Fetches ALL comment pages (--paginate --slurp; gh api returns 30 comments
# per page, and long review threads exceed that) so the true latest bot
# comment is resolved. Body composition is delegated to
# compose_claude_comment.py so both workflow patch steps share one
# implementation.
set -euo pipefail

BOT_LOGIN="${BOT_LOGIN:-claude[bot]}"

# --slurp wraps each page in an outer array; .[][] flattens to comments.
COMMENT=$(gh api --paginate --slurp "repos/${REPO}/issues/${ISSUE_NUMBER}/comments" \
  | jq --arg bot "$BOT_LOGIN" \
      '[.[][] | select(.user.login == $bot)] | sort_by(.updated_at) | last')

COMMENT_ID=$(printf '%s' "$COMMENT" | jq -r '.id')

if [ -z "$COMMENT_ID" ] || [ "$COMMENT_ID" = "null" ]; then
  echo "No ${BOT_LOGIN} comment found — nothing to update."
  exit 0
fi

BODY=$(printf '%s' "$COMMENT" | jq -r '.body')

NEW_BODY=$(BODY_IN="$BODY" python3 .github/scripts/compose_claude_comment.py)

gh api "repos/${REPO}/issues/comments/${COMMENT_ID}" \
  --method PATCH \
  --field body="$NEW_BODY"
