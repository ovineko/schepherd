# schepherd

Schepherd delivers a chosen JSON Schema from a pinned OCI catalog to a
verified local file. Every schema is its own artifact, and the catalog moves
between registries without changing its digest. Schemas of your own
repository can be declared as [local schemas](configuration.md#local-schemas)
and are used in place. What happens with the file afterwards is decided by
your consumer, not by Schepherd.

Start with [installation](installation.md), [configuration](configuration.md)
and the [command reference](cli.md). Read the [security and trust model](security.md)
before relying on a pin, [publishing](publishing.md) if you maintain a
schema set, and [catalog automation](automation.md) if you operate the weekly
catalog update. [Versioning](versioning.md) explains the two release trains:
the client (SemVer) and the schema catalog (time-based revisions).

Schepherd does not validate documents, never contacts upstream schema URLs at
runtime and never executes anything that came from a registry. See
[Architecture](architecture.md) for the full list.
