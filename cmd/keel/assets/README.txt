This directory is the build-time asset root for the self-contained keel binary.

`scripts/package-self-contained.sh` fills it (not committed):
  pg.txz    PostgreSQL 18 runtime tree (xz-compressed tar)
  web/      packaged NSH web client
  icon.png  app icon
  README.md release README

A repository build with only this file has no bundled runtime: `keel start`
falls back to an external PostgreSQL and web/dist, with a readable message.
