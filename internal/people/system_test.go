package people

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

// WhatsApp's own account (0@s.whatsapp.net) is named "WhatsApp" and
// rendered plain; a profile name sent from it never replaces that.
func TestSystemAccountNamed(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	id, err := Resolve(ctx, db, types.NewJID("0", types.DefaultUserServer))
	if err != nil {
		t.Fatal(err)
	}
	if err := SetNames(ctx, db, id, Names{PushName: "Support <script>"}); err != nil {
		t.Fatal(err)
	}
	var name, source string
	if err := db.QueryRow(`SELECT display_name, name_source FROM persons WHERE id = ?`, id).Scan(&name, &source); err != nil {
		t.Fatal(err)
	}
	if name != "WhatsApp" || source != SourceSystem || !TrustedSource(source) {
		t.Fatalf("system account = %q/%q", name, source)
	}
}
