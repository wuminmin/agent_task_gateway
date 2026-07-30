# Legacy RQ4 source snapshot

The legacy RQ4 measurements predate the V4 implementation, so their recorded
source digests must not be compared with the current working tree. The three
compact Git archives in this directory contain exactly the source selections
used by the small-query performance, Control-PG storage, and multiscale
Join--Group campaigns at commit
`38a35d7bb3baff5a6f731b40a42dd4a26f28e29d`.

The paper evidence generator verifies each archive digest and safe member set,
recomputes each campaign's path-framed source digest from the archived bytes,
and separately reconstructs the available legacy metrics from retained raw
artifacts. This preserves historical provenance without relabeling the old
measurements as results from the current V4 source tree.
