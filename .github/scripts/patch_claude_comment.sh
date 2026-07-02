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
#      RUN_ID (optional) — when set, only comments embedding this run's
#      /actions/runs/<RUN_ID> link qualify. github-actions[bot] is a shared
#      author (any workflow posts as it), so latest-by-author alone could
#      stamp a foreign comment; the action's tracking comment always links
#      its own run. Deliberately no fallback on miss: a missing footer is
#      benign, patching another workflow's comment is not.
#
# Fetches ALL comment pages (--paginate --slurp; gh api returns 30 comments
# per page, and long review threads exceed that) so the true latest bot
# comment is resolved. Body composition is delegated to
# compose_claude_comment.py so both workflow patch steps share one
# implementation.
set -euo pipefail

BOT_LOGIN="${BOT_LOGIN:-claude[bot]}"
RUN_ID="${RUN_ID:-}"

# --slurp wraps each page in an outer array; .[][] flattens to comments.
# The run-id match is boundary-anchored so run 22 never matches runs/222.
COMMENT=$(gh api --paginate --slurp "repos/${REPO}/issues/${ISSUE_NUMBER}/comments" \
  | jq --arg bot "$BOT_LOGIN" --arg run "$RUN_ID" \
      '[.[][]
        | select(.user.login == $bot)
        | select($run == "" or (.body | test("/actions/runs/" + $run + "([^0-9]|$)")))]
       | sort_by(.updated_at) | last')

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
