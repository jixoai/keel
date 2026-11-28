# fix-windows-pg-start

Fix Windows v0.6.0 first-start hang: pg_ctl start is captured through a pipe inherited by the daemonized postmaster, so Go waits for EOF forever. Start pg_ctl with file stdio and Run(), add a regression test, bump 0.6.1 and release without touching v0.6.0.
