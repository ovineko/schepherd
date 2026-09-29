package notes

import "github.com/ovineko/schepherd/internal/publisher/state"

// ChangesOf returns what the current revision of st did, from the revisions
// the publisher records per schema: the schemas it (re)added, those whose
// entry it changed with a new artifact or in metadata only (then
// artifactRevision names the older revision of the artifact), those an
// exclude rule removed in it, and the entries it left as they were. Held
// schemas are among the unchanged ones, as in the publish result, and are
// also listed with their reason. Schemas excluded by an earlier revision
// are not in the catalog and not counted.
func ChangesOf(st *state.State) Changes {
	c := Changes{Added: []string{}, Changed: []string{}, MetadataChanged: []string{}, Held: []Held{}, Excluded: []string{}}
	revision := st.Catalog.Revision

	for i := range st.Schemas {
		rec := &st.Schemas[i]

		switch {
		case rec.Excluded():
			if rec.ExcludedRevision == revision {
				c.Excluded = append(c.Excluded, rec.ID)
			}
		case rec.FirstRevision == revision:
			c.Added = append(c.Added, rec.ID)
		case rec.LastChangedRevision == revision && rec.ArtifactRevision != "":
			c.MetadataChanged = append(c.MetadataChanged, rec.ID)
		case rec.LastChangedRevision == revision:
			c.Changed = append(c.Changed, rec.ID)
		default:
			c.Unchanged++

			if rec.Held() {
				c.Held = append(c.Held, Held{ID: rec.ID, Reason: rec.HeldReason})
			}
		}
	}

	return c
}
