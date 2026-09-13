<#
.SYNOPSIS
  Runs the purchase-system load and chaos harness against the local stack.

.DESCRIPTION
  Drives the real pipeline (checkout, PostgreSQL job ledger, RabbitMQ when
  configured, Redis when configured, the Mercado Pago adapter, settlement)
  against a misbehaving stub of Mercado Pago, then audits the invariants money
  depends on. Exits non-zero if any invariant is violated.

  Start the services first:
    docker compose up -d database cache broker

.PARAMETER Orders
  Checkout attempts to make. Demand should exceed supply to exercise sold-out
  refusals and late payments.

.PARAMETER Buyers
  Concurrent buyers.

.PARAMETER Tiers
  Ticket tiers on sale.

.PARAMETER Capacity
  Tickets per tier.

.PARAMETER DbConns
  Connection pool shared by the whole run. Keep it below the server's
  max_connections; buyers beyond it queue on the pool, which is the point.

.EXAMPLE
  .\scripts\loadtest.ps1 -Orders 20000 -Buyers 200 -Tiers 20 -Capacity 500
#>
param(
  [int]$Orders = 5000,
  [int]$Buyers = 100,
  [int]$Tiers = 10,
  [int]$Capacity = 300,
  [int]$Workers = 4,
  [double]$ApproveRate = 0.85,
  [double]$ProviderFailure = 0.05,
  [int]$DuplicateWebhooks = 3,
  [int]$DbConns = 40,
  [switch]$Keep
)

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

$arguments = @(
  "run", "./cmd/loadtest",
  "-orders", $Orders,
  "-buyers", $Buyers,
  "-tiers", $Tiers,
  "-capacity", $Capacity,
  "-workers", $Workers,
  "-approve-rate", $ApproveRate,
  "-provider-failure", $ProviderFailure,
  "-duplicate-webhooks", $DuplicateWebhooks,
  "-db-conns", $DbConns
)
if ($Keep) { $arguments += "-keep" }

& go @arguments
exit $LASTEXITCODE
