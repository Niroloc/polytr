#!/usr/bin/env bash
# Lists active BTC binary option markets on Polymarket with their token IDs.
# Requires: curl, jq

set -euo pipefail

LIMIT=${1:-20}

echo "Fetching Bitcoin markets from Polymarket CLOB..."
echo ""

curl -s "https://clob.polymarket.com/markets?tag=Bitcoin&limit=${LIMIT}&active=true" \
  | jq -r '
    .data[]
    | select(.closed == false)
    | "Market:  \(.question)\n" +
      "EndDate: \(.end_date_iso)\n" +
      (.tokens[] | "  token_id=\(.token_id)  outcome=\(.outcome)") +
      "\n"
  '
