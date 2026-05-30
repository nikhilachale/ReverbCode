# GORM vs sqlc for the Go + SQLite Agent-Orchestrator Backend

## Executive Summary

This report evaluates two Go data-layer approaches for rewriting the agent-orchestrator on a **Go + SQLite** stack: **GORM**, a mature runtime ORM built on reflection, and **sqlc**, a compile-time code generator that turns hand-written SQL into type-safe Go. The two tools sit at opposite ends of the abstraction spectrum. GORM optimizes for *developer velocity and convenience* — you describe structs and GORM writes the SQL — at the cost of runtime reflection overhead, hidden queries, and a class of well-documented footguns (N+1 loading, soft-delete filtering, zero-value update surprises). sqlc optimizes for *explicitness and correctness* — you write the SQL, sqlc verifies it against your schema at build time and generates fully-typed Go — at the cost of needing an external migration tool and some friction with dynamic queries.

For an agent-orchestrator workload (many concurrent sessions, event streams, job queues, heavy mixed reads/writes against a single SQLite file), the dominant constraint is **SQLite's single-writer model**, which is orthogonal to the ORM choice and must be solved with WAL mode, a busy_timeout, and a one-connection write pool regardless. Given that the schema is well-known up front and correctness/observability of SQL matters for a queue/event system, this report recommends **sqlc as the primary data layer**, with hand-written `database/sql` (or sqlx) for the small set of genuinely dynamic queries.

| Dimension | GORM | sqlc |
|---|---|---|
| Approach | Runtime ORM (reflection) | Compile-time codegen from SQL |
| You write | Go structs + method chains | Raw SQL + schema |
| Type safety | Runtime (`interface{}`) | Compile-time (generated structs) |
| SQL visibility | Hidden/generated | Explicit, reviewed |
| Migrations | Built-in AutoMigrate (+ external recommended) | None — bring your own (goose/golang-migrate/atlas) |
| Dynamic queries | Excellent | Limited (needs workarounds) |
| Performance | Reflection overhead | ~ raw database/sql |
| Footguns | N+1, soft-delete, zero-value updates | Dynamic SQL, newer SQLite engine |
| Build pipeline | None | `sqlc generate` step |

---

## 1. Philosophy & Approach

**GORM** is a full-featured runtime ORM. The developer defines Go structs annotated with tags, and GORM uses **reflection** to map those structs to tables, build SQL dynamically, and scan results back. The mental model is object-centric: you think in terms of models and associations (`db.Preload("Sessions").Find(&jobs)`), and SQL is an implementation detail GORM produces for you. This maximizes convenience and lets you write very little code for CRUD, but the actual SQL executed is not visible in your source and is constructed at runtime.

**sqlc** inverts this. You write **hand-authored SQL** in `.sql` files, annotated with `-- name: GetSession :one` directives, plus a `schema.sql` describing your tables. At build time, `sqlc generate` parses both, type-checks the queries against the schema, and emits plain Go functions returning concrete structs. The mental model is SQL-first: the database and its queries are the source of truth, and Go code is a generated, type-safe binding. There is no runtime ORM layer — generated code calls `database/sql` directly.

The core trade is **convenience and dynamism (GORM)** vs **explicitness and compile-time verification (sqlc)**.

## 2. SQLite-Specific Support and Quirks

**Drivers.** Both work with the two main SQLite drivers: `mattn/go-sqlite3` (cgo, the most battle-tested, requires a C toolchain) and `modernc.org/sqlite` (pure-Go, no cgo, simpler cross-compilation, slightly slower historically). GORM ships an official SQLite driver wrapper (`gorm.io/driver/sqlite`, built on mattn by default; a pure-Go variant exists). sqlc is driver-agnostic — it generates `database/sql` code and you register whichever driver you want, making the pure-Go `modernc` route especially clean.

**FTS5 / JSON1.** Because sqlc passes your SQL through largely untouched, FTS5 full-text search and JSON1 functions "just work" as long as the driver's SQLite build includes them (both common builds do). With GORM, FTS5 and advanced JSON queries typically require dropping to `db.Raw(...)`/`db.Exec(...)` since they aren't first-class in the ORM. **Caveat:** sqlc's SQLite parser is newer than its PostgreSQL parser and may not understand every exotic FTS5/virtual-table or JSON construct at codegen time; such queries sometimes need to be written so sqlc can parse them, or handled outside sqlc.

**Concurrency.** SQLite permits only **one writer at a time** regardless of which Go library you use. The standard mitigation — WAL mode, a `busy_timeout`, and capping the write connection pool to a single connection — is identical for both and is covered in section 9.

## 3. Type Safety, Ergonomics, and Refactoring

**sqlc** provides **compile-time type safety**: queries are checked against the schema during `sqlc generate`, and the generated Go uses concrete types. If you rename or drop a column, regeneration fails (or the regenerated code no longer compiles against your call sites), so breakage surfaces **loudly at build time**. This is the strongest argument for sqlc on a long-lived schema: the compiler is your migration safety net, and IDE autocomplete works on real generated structs.

**GORM** resolves much at **runtime via reflection**. A renamed column referenced in a string (`Where("status = ?")`, `Order("created_at")`, `Select("...")`) compiles fine and fails — or silently misbehaves — only when executed. Struct-field changes are safer, but the large surface of string-based query building means refactors can break silently. Ergonomically GORM is terser for CRUD and associations; sqlc requires writing SQL but rewards it with certainty.

## 4. Performance

**sqlc** generates code that calls `database/sql` directly with no reflection, so its overhead is essentially that of the raw driver — minimal allocations, predictable behavior. **GORM** adds reflection-based struct mapping and dynamic SQL construction on every call, which measurably increases CPU and allocations versus raw `database/sql`/sqlc; community benchmarks consistently show GORM as one of the heavier options, though for most workloads the DB round-trip dominates and the difference is not the bottleneck. Both can use prepared statements (GORM via `PrepareStmt: true`; sqlc/`database/sql` via the standard statement cache). For a write-heavy SQLite system the real ceiling is SQLite's single-writer serialization, not ORM CPU — but sqlc's lower overhead and explicit SQL make hot paths easier to reason about and optimize.

## 5. Migration Tooling & Schema Evolution

**This is a key operational difference. sqlc does NOT run migrations** — it only reads a schema definition to type-check queries. You must pair it with an external tool: **goose**, **golang-migrate**, or **atlas**. The typical workflow: write a migration → apply it → ensure sqlc's `schema` input reflects the new schema → `sqlc generate` → fix any now-broken Go. Atlas can even derive migrations from a desired schema and has first-class sqlc integration.

**GORM** ships **AutoMigrate**, which inspects structs and creates/alters tables to match. It's convenient for prototyping but is widely considered insufficient for production: it **adds columns/indexes but does not drop or safely rename** them, offers no down-migrations, and gives limited control over destructive changes. Most serious GORM projects therefore *also* adopt golang-migrate/goose/atlas, narrowing GORM's apparent advantage here. Net: both realistically use an external migration tool; sqlc just makes that mandatory and explicit.

## 6. Testing Story

Both test best against **real in-memory SQLite** (`:memory:` or a temp file), which is fast and exercises real SQL. Because sqlc generates an interface (`Querier`) and concrete `Queries` type, you can either (a) run queries against a real in-memory DB for integration tests, or (b) mock the generated `Querier` interface for unit tests of business logic — a clean, idiomatic split. GORM is harder to mock at the SQL level (`go-sqlmock` is brittle against GORM's generated SQL); the pragmatic approach is real SQLite. Overall sqlc's generated interfaces give a more natural mocking seam, while both favor real-SQLite integration tests with fixtures. **SQLite `:memory:` caveat:** each connection gets its own database, so use a shared-cache DSN or a single connection in tests.

## 7. Code Generation / Build Pipeline Implications (sqlc)

Adopting sqlc adds a **build step**. You maintain `sqlc.yaml` (engine = sqlite, paths to `schema` and `queries`), write SQL with `-- name:` annotations, and run `sqlc generate` to emit Go. Implications:

- **Regeneration discipline:** any schema or query change requires re-running `sqlc generate`; forgetting to commit regenerated code, or schema drift between the migration and sqlc's schema input, is the main friction point. CI should run `sqlc generate` and fail if the working tree changes (drift check), plus `sqlc vet`.
- **Build breakage is a feature:** an invalid query or a column that no longer exists fails generation, catching errors before runtime.
- **Tooling cost:** contributors need the sqlc binary (pinned version) installed/available in CI. Generated files are checked in.

GORM has **no codegen step** — lower setup friction, at the cost of moving error detection to runtime.

## 8. Community Health, Maintenance, Known Footguns

Both projects are **healthy and widely used** (GORM and sqlc each have tens of thousands of GitHub stars, active maintenance, and frequent releases as of 2026). GORM is the most popular Go ORM; sqlc is the leading SQL-first codegen tool.

**GORM footguns (well-documented):**
- **N+1 queries:** lazy/association access without `Preload`/`Joins` issues a query per row; you must remember to eager-load.
- **Soft delete:** a `gorm.DeletedAt` field silently turns every query into `WHERE deleted_at IS NULL`. Forgetting this surprises people ("rows are gone"); you need `Unscoped()` to see/really-delete them. A genuine, common gotcha.
- **Zero-value updates:** `Updates` with a struct **skips zero-valued fields** (0, "", false), so updating a field to its zero value silently does nothing — you must use a `map` or `Select` to force it. Classic source of bugs.
- Hooks and implicit transactions add hidden behavior.

**sqlc limitations/footguns:**
- **Dynamic SQL:** sqlc generates static queries; **variable-length `IN (...)`**, runtime-built `WHERE`/sorting, and conditional filters don't map cleanly. Workarounds: `sqlc.slice()` (supported for some engines/drivers), SQLite `json_each`-based array params, generating per-shape queries, or dropping to hand-written `database/sql` for those cases.
- **SQLite engine maturity:** sqlc's SQLite support is newer than PostgreSQL; some SQL features/functions may not parse and need rewording.
- Less convenient for sprawling ad-hoc query shapes.

## 9. Best Fit for the Agent-Orchestrator Workload

The workload — **many concurrent sessions, event streams, job queues, heavy mixed reads/writes on one SQLite file** — is dominated by SQLite's concurrency model, not the ORM:

- **Single writer:** SQLite serializes writes. Required setup (same for both tools): enable **WAL** (`PRAGMA journal_mode=WAL`) for concurrent readers alongside one writer, set a **`busy_timeout`** (e.g. 5000ms) to avoid `SQLITE_BUSY`, and **cap the write pool to one connection** (`db.SetMaxOpenConns(1)` for the writer, or a dedicated writer connection with a separate read pool). This is the single most important decision and is independent of GORM vs sqlc.
- **Job queue / event semantics:** queue claim patterns (`UPDATE ... RETURNING`, `SELECT ... LIMIT` with status filters), batch inserts for event streams, and careful transaction scoping benefit from **explicit, reviewable SQL** — sqlc's strength. You can see and tune exactly what hits the disk.
- **Dynamic filtering:** session/event list endpoints with optional filters are where **GORM shines** and sqlc needs workarounds. In practice this is a small, contained part of the surface and can use hand-written SQL.
- **Performance headroom:** sqlc's near-zero overhead is a mild plus on hot read paths, but again the writer is the bottleneck.

**Verdict for this workload:** the queue/event core favors **explicit SQL (sqlc)** for correctness and observability; the dynamic-listing edges favor GORM-style flexibility, which can be met with a thin hand-written-SQL escape hatch.

## 10. Alternatives Worth Flagging (brief)

- **sqlx** — a thin extension of `database/sql` (named params, struct scanning) with **no codegen and no ORM**. Best when you want hand-written SQL like sqlc but prefer zero build step and full runtime dynamism; you give up sqlc's compile-time query checking. A natural companion to sqlc for the dynamic-query escape hatch.
- **ent** (ent.io) — a schema-as-Go-code graph ORM with strong codegen, type-safe traversals, and great support for complex relations/graphs. Heavier and opinionated; shines when your domain is highly relational. SQLite supported.
- **bun** — a lightweight SQL-first ORM (successor to go-pg) with a fluent query builder, good performance, migrations, and multi-DB support including SQLite. A middle ground between GORM's convenience and sqlc's explicitness if you want a builder without full codegen.
- **jet** — like sqlc in spirit (codegen, type-safe) but generates a **type-safe query *builder*** from your DB schema rather than from hand-written SQL, giving compile-time-checked **dynamic** queries. Worth a look precisely where sqlc struggles (dynamic filters) while keeping type safety.

## 11. Recommendation & Rationale

**Recommendation: adopt sqlc as the primary data layer**, paired with an external migration tool (**goose** or **golang-migrate**, or **atlas** if you want schema-diff-driven migrations) and a thin **hand-written `database/sql`/sqlx** escape hatch for the handful of genuinely dynamic queries (filtered list endpoints, variable `IN` clauses).

**Why sqlc fits this project:**
- The schema is known up front and long-lived; **compile-time query checking** turns schema evolution into a build-time safety net rather than a runtime risk — valuable for a queue/event system where a silent bad query is costly.
- A job-queue/event-stream core benefits from **explicit, reviewable, tunable SQL** and **near-zero runtime overhead**.
- Generated **`Querier` interfaces** give a clean testing/mocking seam, and the driver-agnostic output makes the pure-Go `modernc.org/sqlite` driver (no cgo) easy to adopt.
- It sidesteps GORM's documented footguns (N+1, soft-delete filtering, zero-value updates) entirely.

**When GORM (or another choice) would be better:**
- If the team prioritizes **raw development velocity** and lots of **ad-hoc/dynamic queries** over compile-time guarantees, GORM's convenience wins and the build step disappears.
- If the domain is **highly relational with complex graph traversals**, **ent** is a stronger fit.
- If you want **type-safe *dynamic* queries** (sqlc's weak spot) without a runtime ORM, evaluate **jet**.
- If you want SQL-first with a fluent builder and built-in migrations in one package, **bun** is a reasonable middle path.

Whatever the choice, **the SQLite concurrency setup (WAL + busy_timeout + single-writer pool) is mandatory and matters more than the ORM decision.**

## TL;DR

**Use sqlc** for the Go + SQLite agent-orchestrator. You get compile-time-checked, explicit, low-overhead SQL — ideal for a job-queue/event-stream core with a stable schema — plus clean generated interfaces for testing. Pair it with **goose/golang-migrate/atlas** for migrations (sqlc does none) and a small **sqlx/`database/sql`** escape hatch for dynamic filter queries. Choose **GORM** instead only if development velocity and pervasive dynamic queries outweigh compile-time safety; consider **ent** for graph-heavy domains or **jet** for type-safe dynamic queries. Regardless of ORM, the decisive lever is SQLite tuning: **WAL mode + busy_timeout + a single writer connection.**

## References

- https://gorm.io/docs/
- https://docs.sqlc.dev/
- https://github.com/go-gorm/gorm
- https://github.com/sqlc-dev/sqlc
- https://pkg.go.dev/modernc.org/sqlite
- https://github.com/mattn/go-sqlite3
- https://github.com/pressly/goose
- https://github.com/golang-migrate/migrate
- https://atlasgo.io/
- https://github.com/jmoiron/sqlx
- https://entgo.io/
- https://bun.uptrace.dev/
- https://github.com/go-jet/jet
