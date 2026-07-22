# TLC results

`make formal` writes the complete TLC log and a provenance-bearing `tlc.json`
status here.  The repository intentionally retains `tlc.log` and `tlc.json`:
the local ignore file explicitly unignores both artifacts while ignoring any
other checker scratch output.  The paper pipeline reports no formal result when
either retained file is absent, its recorded digest is stale, or a claimed pass
lacks recognizable final statistics and TLC's no-error completion marker.
