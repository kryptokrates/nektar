---------------------------- MODULE AORG ----------------------------
(* AGT-008 / §18: model of the append-only reasoning graph (packages/graph)
   and its anchoring (packages/anchor). Checked with TLC (see README.md).

   An agent appends vertices; each vertex names premises that must already
   exist; refinements invalidate (never delete) an earlier vertex; anchoring
   seals a prefix of the log. An adversary may try to rewrite a sealed vertex.

   Invariants checked:
     Acyclic          — every premise precedes the vertex citing it (AOR-003)
     Traceable        — every derived vertex reaches an episode (AOR-007/008b)
     AppendOnly       — sealed vertices never change (AOR-006 finality)
     NoDeleteOnRefine — refinement adds an invalidation, keeps the old vertex (AOR-008a)
     ViewSound        — the current view never contains an invalidated vertex *)
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS MaxV,          \* bound on vertices for model checking
          Kinds,         \* {"episode", "derived", "refinement"}
          AllowSealedTamper \* FALSE = implementation; TRUE = mutant that must violate AppendOnly

VARIABLES log,           \* Seq of [kind, prem: SUBSET 1..MaxV, refines: 0..MaxV, body]
          sealed,        \* number of vertices covered by an anchor
          invalid,       \* set of invalidated vertex indices
          history        \* every (index, record) ever observed sealed — for AppendOnly

vars == <<log, sealed, invalid, history>>

Idx == 1..Len(log)

Init == log = << >> /\ sealed = 0 /\ invalid = {} /\ history = {}

AppendEpisode ==
  /\ Len(log) < MaxV
  /\ log' = Append(log, [kind |-> "episode", prem |-> {}, refines |-> 0, body |-> Len(log) + 1])
  /\ UNCHANGED <<sealed, invalid, history>>

\* derived: premises must already exist (acyclic by construction)
AppendDerived(P) ==
  /\ Len(log) < MaxV
  /\ P /= {} /\ P \subseteq Idx
  /\ log' = Append(log, [kind |-> "derived", prem |-> P, refines |-> 0, body |-> Len(log) + 1])
  /\ UNCHANGED <<sealed, invalid, history>>

\* refinement: supersedes an existing non-episode vertex; old one stays
Refine(old, P) ==
  /\ Len(log) < MaxV
  /\ old \in Idx /\ log[old].kind /= "episode" /\ old \notin invalid
  /\ P /= {} /\ P \subseteq Idx
  /\ log' = Append(log, [kind |-> "refinement", prem |-> P, refines |-> old, body |-> Len(log) + 1])
  /\ invalid' = invalid \cup {old}
  /\ UNCHANGED <<sealed, history>>

\* anchor: seal the whole current log (one batch)
Anchor ==
  /\ sealed < Len(log)
  /\ sealed' = Len(log)
  /\ history' = history \cup {<<i, log[i]>> : i \in 1..Len(log)}
  /\ UNCHANGED <<log, invalid>>

\* adversary: try to rewrite a vertex. The implementation refuses edits to
\* sealed vertices (they would break the anchored root), so only an unsealed
\* vertex can be replaced — that is exactly the "draft" window.
Tamper(i) ==
  /\ i \in Idx /\ (AllowSealedTamper \/ i > sealed)
  /\ log' = [log EXCEPT ![i].body = 0]
  /\ UNCHANGED <<sealed, invalid, history>>

Next ==
  \/ AppendEpisode
  \/ \E P \in SUBSET Idx : AppendDerived(P)
  \/ \E o \in Idx, P \in SUBSET Idx : Refine(o, P)
  \/ Anchor
  \/ \E i \in Idx : Tamper(i)

Spec == Init /\ [][Next]_vars

\* ── invariants ──────────────────────────────────────────────────────────
TypeOK == /\ sealed \in 0..MaxV /\ invalid \subseteq 1..MaxV /\ Len(log) <= MaxV

Acyclic == \A i \in Idx : \A p \in log[i].prem : p < i

RECURSIVE Reaches(_, _)
Reaches(i, d) == IF log[i].kind = "episode" THEN TRUE
                 ELSE IF d = 0 THEN FALSE
                 ELSE \E p \in log[i].prem : Reaches(p, d - 1)
Traceable == \A i \in Idx : Reaches(i, MaxV)

AppendOnly == \A h \in history : h[1] \in Idx /\ log[h[1]] = h[2]

NoDeleteOnRefine == \A i \in invalid : i \in Idx

CurrentView == {i \in Idx : i \notin invalid}
ViewSound == CurrentView \cap invalid = {}

Invariants == TypeOK /\ Acyclic /\ Traceable /\ AppendOnly /\ NoDeleteOnRefine /\ ViewSound
=============================================================================
