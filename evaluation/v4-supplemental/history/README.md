# Supplemental campaign source history

This directory is a provenance overlay for the immutable files in
`../evidence/`; it does not replace or rewrite that sealed evidence bundle.

`historical-source-fede479.tar.gz` was reconstructed after the campaign with
deterministic `git archive` from retained commit
`fede4798add8bb7bbf5793466efc9cf857c4bb8a`. The accompanying JSON records the
commit, tree, archive, path-set, and generalized source-scope digests. The
paper validator safely parses the archive and independently reconstructs the
generalized, concurrency, oracle-repository, and oracle-package bindings that
the original reports recorded.

Later source changes are reported as current-tree divergence. They do not
rewrite the July 30 measurements or make those measurements evidence for the
later implementation.
