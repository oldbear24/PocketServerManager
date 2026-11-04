package audit

import (
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/oldbear24/PocketServerManager/internal/schema"
)

// Rehash recomputes the hash of every entry from seq on, and the head's seq and
// hash, the way someone who knows the scheme would after editing an entry. It
// cannot recompute the seal, which needs the master key.
func Rehash(app core.App, seq int) error {
	collection, err := app.FindCollectionByNameOrId(schema.AuditLog)
	if err != nil {
		return err
	}

	rows := []dbx.NullStringMap{}
	if err := app.RecordQuery(collection).OrderBy("[[seq]] ASC").All(&rows); err != nil {
		return err
	}

	prevHash := ""
	for _, row := range rows {
		record := core.NewRecord(collection)
		record.Load(rowValues(collection, row))

		if record.GetInt("seq") >= seq {
			canonical, err := canonicalEntry(record)
			if err != nil {
				return err
			}
			hash := entryHash(prevHash, canonical)
			_, err = app.DB().NewQuery("UPDATE {{" + schema.AuditLog + "}} SET [[prev_hash]] = {:prev}, [[hash]] = {:hash} WHERE [[seq]] = {:seq}").
				Bind(dbx.Params{"prev": prevHash, "hash": hash, "seq": record.GetInt("seq")}).Execute()
			if err != nil {
				return err
			}
			prevHash = hash
		} else {
			prevHash = record.GetString("hash")
		}
	}

	last, lastHash, err := tail(app.DB())
	if err != nil {
		return err
	}
	_, err = app.DB().NewQuery("UPDATE {{" + HeadTable + "}} SET [[seq]] = {:seq}, [[hash]] = {:hash}").
		Bind(dbx.Params{"seq": last, "hash": lastHash}).Execute()
	return err
}
