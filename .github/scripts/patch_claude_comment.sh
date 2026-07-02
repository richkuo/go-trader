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
#      its own run. Deliberately no PATCH fallback on miss: a missing footer
#      is benign, patching another workflow's comment is not.
#      ON_MISS (optional, default skip) — "post" makes a miss post a NEW
#      comment carrying STATUS_NOTE instead of exiting silently. Set by the
#      cancellation/failure step: a failure before the action creates its
#      tracking comment (checkout, uv sync, prompt composition) must still
#      surface in the thread — the only channel the user watches — without
#      misattributing the note to an older run's comment. Requires a
#      non-empty STATUS_NOTE (a footer-only comment is noise).
#
# Fetches ALL comment pages (--paginate --slurp; gh api returns 30 comments
# per page, and long review threads exceed that) so the true latest bot
# comment is resolved. Body composition is delegated to
# compose_claude_comment.py so both workflow patch steps share one
# implementation.
set -euo pipefail

BOT_LOGIN="${BOT_LOGIN:-claude[bot]}"
RUN_ID="${RUN_ID:-}"
ON_MISS="${ON_MISS:-skip}"

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
