package backup

import "testing"

func TestTheGooseVersionIsReplayedTheWayGooseDoesIt(t *testing.T) {
	copyBlock := func(rows ...string) string {
		out := "--\n-- Data for Name: goose_db_version\n--\n\nCOPY public.goose_db_version (id, version_id, is_applied, tstamp) FROM stdin;\n"
		for _, r := range rows {
			out += r + "\n"
		}
		return out + "\\.\n\n"
	}
	for name, c := range map[string]struct {
		out  string
		want int64
	}{
		"applied in order":            {copyBlock("1\t0\tt\t2026-01-01", "2\t1\tt\t2026-01-01", "3\t2\tt\t2026-01-01"), 2},
		"a rolled-back one is undone": {copyBlock("1\t0\tt\tx", "2\t1\tt\tx", "3\t2\tt\tx", "4\t2\tf\tx"), 1},
		"re-applied after a rollback": {copyBlock("1\t0\tt\tx", "2\t1\tt\tx", "3\t1\tf\tx", "4\t1\tt\tx"), 1},
		"rows out of order":           {copyBlock("3\t2\tt\tx", "1\t0\tt\tx", "2\t1\tt\tx"), 2},
	} {
		if got, err := gooseVersion(c.out); err != nil || got != c.want {
			t.Errorf("%s: version = %d, %v; want %d", name, got, err, c.want)
		}
	}
	for name, out := range map[string]string{"no table": "", "no rows": copyBlock(), "a short row": copyBlock("1\t0")} {
		if _, err := gooseVersion(out); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestTheSchemaOfATableIsReadFromPgRestoresList(t *testing.T) {
	list := `;
; Archive created at 2026-11-23 10:00:00 UTC
;     dbname: clipmux
215; 1259 16419 TABLE public goose_db_version clipmux
217; 1259 16420 SEQUENCE public goose_db_version_id_seq clipmux
230; 1259 16500 TABLE t_ab12 projects clipmux
`
	if got := tableSchema(list, "goose_db_version"); got != "public" {
		t.Errorf("schema = %q, want public (the sequence of the same name is not the table)", got)
	}
	if got := tableSchema(list, "projects"); got != "t_ab12" {
		t.Errorf("schema = %q, want t_ab12", got)
	}
	if got := tableSchema(list, "nothing"); got != "" {
		t.Errorf("schema = %q, want none", got)
	}
}
