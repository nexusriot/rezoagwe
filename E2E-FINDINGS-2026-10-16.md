
## 5. Two counters that reached no screen, and a second flaky test

**The guards were invisible.** `chat_unsolicited_total` and
`chat_self_attributed_total` were added with the two repair guards and exported
to Prometheus, but appeared in no UI — not the TUI, not the Android metrics
screen, not the desktop counters — and raised no finding. They are the only
evidence that some peer is sending malformed repairs, which is precisely the
kind of thing this project refuses to leave to a log nobody reads. Both now
show in all three counter panes, and a non-zero count raises a **warn**: the
frames were refused, so nothing is broken, but neither has a legitimate cause.

**`NodeEngineTest.aReportSaysWhichSideHoldsATombstone` flaked**, once in eight
full-suite runs — yet passed 12 times in a row when run alone. That split is
the tell: the cause is between tests, not in the test.

`freePort()` asked the kernel for a port, closed it, and returned the number.
Nothing holds it between that call and the `start()` that binds it, and nothing
recorded what had already been issued — so two engines in one test could be
handed the *same* port. The second fails to bind, that node is dead, and
whichever assertion depended on it fails looking like a protocol bug. The
helper's own comment already described this shape of failure for the TCP/UDP
half ("failed as a BindException in an unrelated test"); the duplicate-issue
half was still open. It now remembers what it has handed out. Eight full-suite
runs green afterwards.
