# fix-serve-ready-schema

Fresh keel serve reports /v1/ready 503 forever because store.CheckReady requires the handoff audit tables and only cmd/runtime applies handoff.NewPGRegistry. Apply the same schema in the product shell, add a product E2E readiness wait, ship with v0.6.1.
