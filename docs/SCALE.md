# Production scale and launch gate

Vozkot Tickets has application-level protection for concurrent on-sales:
atomic inventory reservation, idempotent checkout and provider calls, a
transactional job ledger, RabbitMQ delivery, bounded workers, payment
reconciliation and fail-fast origin admission.

This is production-shaped application code, not a claim that the development
Compose stack can accept a million simultaneous buyers. Millions of tickets
can live in an event; millions of buyers arriving together must be paced at the
edge and proven on production-sized infrastructure.

## Target topology

```text
buyers
  │
  ▼
Cloudflare Waiting Room ──▶ load balancer ──▶ API replicas
                                                │
                       Redis ◀── per-buyer limit┤
                                                ▼
                                    PostgreSQL primary
                              order + stock + job commit together
                                                │
                               bounded async publish after commit
                                                ▼
                                    RabbitMQ quorum queue
                                                │
                                    bounded worker replicas
                                                ▼
                                         Mercado Pago
                                                │
                           webhook + oldest-first reconciliation
```

PostgreSQL is the business ledger and recovery source. RabbitMQ is the fast
transport. This is the transactional outbox pattern: if publication fails, the
committed row is still claimed by the poller; redelivery is safe because claims
and payment creation are idempotent.

References:

- [AWS transactional outbox guidance](https://docs.aws.amazon.com/prescriptive-guidance/latest/cloud-design-patterns/transactional-outbox.html)
- [PostgreSQL `SKIP LOCKED`](https://www.postgresql.org/docs/current/sql-select.html)
- [RabbitMQ confirms and acknowledgements](https://www.rabbitmq.com/docs/confirms)
- [RabbitMQ quorum queues](https://www.rabbitmq.com/docs/quorum-queues)
- [Cloudflare Waiting Room](https://developers.cloudflare.com/waiting-room/about/)

## Verified local acceptance run

Run date: 2026-09-13. Real local PostgreSQL, Redis and RabbitMQ; four workers;
60 database connections; 200 concurrent buyers; stub provider at 400 ms; 2%
transient provider failures; three webhook copies per payment; 1% of payments
lost every webhook copy.

```text
checkout attempts                         20,000
orders opened                              5,928
sold-out refusals                         14,072
throughput                                 2,405 attempts/s
latency                                    p50 37 ms / p95 301 ms / p99 507 ms
charges                                    5,928 in 30.084 s
provider outcome                           5,041 approved / 453 rejected / 434 expired
webhooks accepted                          17,610 of 17,784
payments with every webhook lost           58
payments recovered by reconciliation       58 of 58 in 6.349 s
PostgreSQL statements refused              0
oversells / lost orders / double charges   0 / 0 / 0
result                                     ALL INVARIANTS HOLD
```

This is a regression baseline from one development machine, not a cloud
capacity promise. Production capacity includes real TLS/network latency,
RabbitMQ replication, managed database limits and Mercado Pago rate limits.

## Relevant settings

```text
CHECKOUT_MAX_IN_FLIGHT=20       # per API replica; outermost checkout bulkhead
RABBITMQ_QUEUE_TYPE=quorum      # production; classic is for local single-node dev
RABBITMQ_PREFETCH=32            # in-flight bound per worker
QUEUE_WORKERS=4                 # per process
QUEUE_BATCH_SIZE=20             # poller bound per worker
QUEUE_RECONCILE_BATCH_SIZE=1000
QUEUE_JOB_TIMEOUT=2m
QUEUE_COMPLETED_RETENTION=168h  # completed delivery rows only
QUEUE_CLEANUP_BATCH_SIZE=10000
```

An existing classic RabbitMQ queue cannot be converted by redeclaring it. Use
a fresh production topology or a controlled migration before setting `quorum`.
Dead jobs, orders and payment fields are not removed by retention cleanup.

## Launch gate

- [ ] Waiting Room protects event and checkout routes and is calibrated below
  the deployed origin's tested rate.
- [ ] RabbitMQ is a three-node or managed HA cluster using quorum queues, with
  ready/unacknowledged/dead-message alarms.
- [ ] PostgreSQL has PgBouncer, HA/failover, automated backups, point-in-time
  recovery and a successful restore drill.
- [ ] Redis has HA/failover and latency/eviction alarms.
- [ ] Mercado Pago has approved the expected request rate and production-like
  payment, webhook, idempotency and refund tests have passed.
- [ ] Dashboards page on checkout latency/503s, database pool waits and locks,
  oldest pending job, dead jobs, reconciliation lag and payment/order mismatch.
- [ ] Staged tests progress through 1k, 10k, 100k and the target virtual-user
  count, followed by a long soak for retention/vacuum behaviour.
- [ ] Chaos tests cover loss of an API, worker, RabbitMQ node, Redis primary and
  PostgreSQL failover.
- [ ] A deliberately hot ticket tier meets the desired admitted checkout rate;
  if not, inventory is partitioned into buckets and re-audited.
- [ ] Operations can pause admission, reconcile payments, handle dead jobs,
  refund late payments and close an event.

Application correctness is the first gate. Passing every infrastructure and
operations item above certifies a particular deployment for a particular
on-sale.
