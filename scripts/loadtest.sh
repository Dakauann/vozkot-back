#!/usr/bin/env sh
# Runs the purchase-system load and chaos harness against the local stack.
#
# Drives the real pipeline (checkout, PostgreSQL job ledger, RabbitMQ when
# configured, Redis when configured, the Mercado Pago adapter, settlement)
# against a misbehaving stub of Mercado Pago, then audits the invariants money
# depends on. Exits non-zero if any invariant is violated.
#
# Start the services first:
#   docker compose up -d database cache broker
#
# Usage:
#   scripts/loadtest.sh                       # defaults: 5000 orders, 100 buyers
#   ORDERS=20000 BUYERS=200 TIERS=20 CAPACITY=500 scripts/loadtest.sh
#
# DB_CONNS (default 40) is the pool the whole run shares; keep it below the
# server's max_connections. Buyers beyond it queue on the pool, which is the
# behaviour under test.
set -eu
cd "$(dirname "$0")/.."

exec go run ./cmd/loadtest \
  -orders "${ORDERS:-5000}" \
  -buyers "${BUYERS:-100}" \
  -tiers "${TIERS:-10}" \
  -capacity "${CAPACITY:-300}" \
  -workers "${WORKERS:-4}" \
  -approve-rate "${APPROVE_RATE:-0.85}" \
  -provider-failure "${PROVIDER_FAILURE:-0.05}" \
  -duplicate-webhooks "${DUPLICATE_WEBHOOKS:-3}" \
  -db-conns "${DB_CONNS:-40}" \
  "$@"
