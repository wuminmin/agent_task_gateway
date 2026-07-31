# TaskGate, Database Provenance, and Authorization

## 1. Position in one sentence

Database provenance explains the origin and derivation of query results.
TaskGate uses a deliberately restricted, provenance-inspired dependency
footprint as one input to a different mechanism: cumulative, task-scoped
exposure admission across an adaptive sequence of database queries.

The two areas are complementary. TaskGate is not proposed as a replacement for
a general provenance system, and ProvSQL is not treated as a competing
task-budget enforcement product.

## 2. Why- and how-provenance

For an output tuple, **why-provenance** identifies source tuples or witnesses
whose presence supports that output. Buneman, Khanna, and Tan introduced the
why/where distinction: where-provenance follows source locations from which an
output value was copied, while why-provenance records contributing source
tuples or witnesses
([ICDT 2001](https://doi.org/10.1007/3-540-44503-X_20)).

**How-provenance** retains algebraic derivation structure rather than only an
unstructured contributor set. In the provenance-semiring account, input
tokens annotate tuples, addition represents alternative derivations, and
multiplication represents joint use in a derivation. Provenance polynomials
can therefore distinguish *how* the same output was obtained
([Green, Karvounarakis, and Tannen, PODS 2007](https://doi.org/10.1145/1265530.1265535)).
The broader terminology and its limits are surveyed by Cheney, Chiticariu, and
Tan ([Foundations and Trends in Databases, 2009](https://doi.org/10.1561/1900000006)).

These formalisms are substantially richer explanation objects than a plain
set of contributing rows. They are foundations TaskGate can learn from; the
TKDE contribution does not claim to invent why- or how-provenance.

## 3. ProvSQL

ProvSQL is a PostgreSQL extension for provenance and probability management.
It rewrites queries through a planner hook, associates output tuples with
provenance-circuit identifiers, and supports semiring evaluation,
where-provenance, aggregation provenance, probability, and other analyses. See
the original system paper
([PVLDB 2018](https://doi.org/10.14778/3229863.3236253)), the current
[ICDE 2026 system paper (arXiv version)](https://arxiv.org/abs/2504.12058),
[ProvSQL introduction](https://provsql.org/docs/user/introduction.html), and
the project's [publication record](https://provsql.org/publications/).

ProvSQL's circuit can be persisted, inspected, and evaluated in multiple ways;
it would therefore be inaccurate to describe it as merely a transient list of
row IDs. Its central abstraction remains a circuit/annotation for database
derivations. The cited ProvSQL interfaces do not define TaskGate's signed task
tuple, three independently bounded exposure sets, shared root-family ledger,
or atomic over-budget reject-before-release transition.

TaskGate, conversely, does not expose a general provenance circuit and cannot
answer every why/how-provenance question. Its dependency component is a
conservative positive-output dependency footprint for a closed operator
profile. It is used for charging, not presented as a minimal causal
explanation.

## 4. Different semantic objects

| Framework | Primary object | Scope of state | Primary question |
|---|---|---|---|
| Why-provenance | Witnesses or contributing source tuples for an output | A result under defined query semantics | Why is this output present? |
| How-/semiring provenance | Algebraic annotation or polynomial describing alternatives and joint derivation | A result and its compositional derivation | How was this output derived? |
| ProvSQL | PostgreSQL provenance circuit referenced from result tuples | Provenance-bearing database results and circuit storage | What is the result's provenance, and how can it be evaluated? |
| TaskGate | Three-dimensional FactID effect plus a monotone root-family ledger | All successfully settled queries in one approved task family and publication | Does this query's *novel cumulative* exposure still fit the approved budget? |

For TaskGate, a query has

\[
E(T,q)=(E_R,E_D,E_O),
\]

where \(E_R\) records candidate released values, \(E_D\) records the
positive-output dependency footprint, and \(E_O\) records the normalized
successful proposition/result. Given the task ledger
\(K=(K_R,K_D,K_O)\), only

\[
\Delta_j=E_j\setminus K_j
\]

is novel. TaskGate admits the buffered result only when
\(|K_j\cup E_j|\le B_j\) for every dimension and then atomically publishes all
three new heads. That cross-query admission state—not the existence of a
provenance annotation—is the distinction.

### Why the distinction matters

An agent can issue individually legal queries that overlap, paginate through a
relation, use supported canonical rewrites, or delegate work to child agents.
A provenance system can explain each resulting tuple. TaskGate additionally
asks whether the union of the resulting task effects contains new facts and
whether that union remains within a human-approved capacity. Repeated facts
are deduplicated by semantic FactID inside the root family; a different
normalized successful proposition still adds an outcome fact even when it
returns an empty result.

Neither property makes TaskGate a general inference-control system. It does
not quantify information content, model the agent's background knowledge, or
provide differential privacy.

## 5. Non-competition statement

A ProvSQL comparison in the TaskGate evaluation is a *boundary and mechanism
study*, not a winner/loser benchmark. Appropriate questions include:

- what artifact each system constructs for one supported query;
- the latency and storage cost of that artifact under a disclosed setup; and
- whether the tested interface provides a root-task ledger and pre-release
  multi-query budget decision.

Raw latency is not an overall quality ranking because the systems compute
different objects and ProvSQL intentionally supports provenance analyses that
TaskGate does not. A result table must therefore report workloads, versions,
configuration, and semantic coverage, leave unmeasured cells blank, and avoid
claims that one system supersedes the other.

## 6. Relationship to database access control

Database access control remains the first gate. PostgreSQL row-level security
uses role/command-specific policy expressions to decide which rows are visible
or modifiable ([PostgreSQL 16 documentation](https://www.postgresql.org/docs/16/ddl-rowsecurity.html)).
Oracle Virtual Private Database attaches dynamic predicates to protected
tables, views, or synonyms to enforce row- and column-level policy
([Oracle Database Security Guide](https://docs.oracle.com/en/database/oracle/oracle-database/26/dbseg/using-oracle-vpd-to-control-data-access.html)).
XACML expresses attribute-based authorization decisions over subject,
resource, action, and environment attributes
([OASIS XACML 3.0](https://docs.oasis-open.org/xacml/3.0/xacml-3.0-core-spec-en.html)).

These mechanisms answer a request-time question such as “may this principal
perform this action on these rows?” They may be combined with external
history-aware policy, but their documented base abstractions do not by
themselves create TaskGate's typed R/D/O FactIDs or compute exact novelty over
an approved multi-query root family. TaskGate therefore composes with RLS,
VPD, or ABAC; it does not weaken or replace them.

## 7. Relationship to agent and tool authorization

Agent identity, tool allowlists, delegated credentials, and MCP authorization
control which tool endpoint an agent may invoke and under which scopes. The MCP
authorization specification, for example, treats an MCP server as an OAuth
resource server and requires audience-bound access-token validation
([MCP authorization specification, 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization)).

That outer authorization boundary is necessary, but a successful tool call
does not by itself identify the database facts represented in a result, merge
them with earlier queries, or decide whether a task-level exposure budget has
been exhausted. TaskGate places that database-semantic accounting boundary
behind the authorized tool. In short:

\[
\text{tool authorization: “may this call reach the service?”}
\]

\[
\text{database authorization: “may this query access these data?”}
\]

\[
\text{TaskGate: “what novel exposure would this task accumulate?”}
\]

All three checks can be required for the same execution.

## 8. Claim boundary

- TaskGate borrows provenance ideas for stable source identity and
  positive-output dependency propagation; it claims neither a new general
  provenance algebra nor feature parity with ProvSQL.
- The dependency footprint is exact only relative to TaskGate's declared
  inductive rules. It is not minimal causality, full physical lineage, or a
  complete account of negative and order information.
- TaskGate's cumulative guarantee ends at the approved root family and fixed
  publication epoch. Separately approved roots and later publications are not
  globally deduplicated.
- Access control and tool authorization remain complementary enforcement
  layers. Exposure accounting cannot grant an otherwise forbidden query.

The exact TaskGate definitions and assumptions are in
[formal-model.md](formal-model.md) and
[exposure-algebra-v2.md](exposure-algebra-v2.md).
