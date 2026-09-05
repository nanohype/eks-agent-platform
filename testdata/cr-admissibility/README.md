# Admissibility corpus

One custom resource per file, each carrying what the API server does to it:

```
# admissibility: admitted
# admissibility: refused     <rule> <path>
# admissibility: pruned      <rule> <path>
# admissibility: cel-refused <rule> <path>
```

`cel-refused` is the gap held open: the API server refuses it for a CEL rule and
the walker is required to say nothing about it, because a rule is a program and
the API server is what runs one. A limit with a case behind it is measured; a
limit in a docstring goes stale in silence.

Two readers hold the same header. `scripts/check-cr-admissibility.py` requires
the shared walker to produce exactly the declared findings and nothing else.
`operators/test/admissibility` creates the same file on a real API server and
requires the same verdict — a refused case rejected with that path in the
message, a pruned case created with that path gone, an admitted case created.

Neither reader can drift without the other failing, and the header is checked
against a running control plane rather than against what its author believed.

A fixture declares every finding the walker makes on it, so a case carrying an
unintended second defect fails rather than passing on a coincidence.
