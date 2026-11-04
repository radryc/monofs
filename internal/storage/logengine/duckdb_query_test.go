package logengine

import "testing"

func TestNormalizeDuckDBSQLAllowsReadOnlyQueries(t *testing.T) {
	cases := []string{
		"SELECT service, count(*) FROM logs GROUP BY 1",
		"WITH recent AS (SELECT * FROM traces WHERE service = 'guardian') SELECT count(*) FROM recent",
		"SELECT l.service, m.metric_name FROM logs l JOIN metrics m ON l.service = m.service",
		"select * from logs;",
		"SELECT * FROM logs WHERE raw_message ILIKE '%delete%'",
		"SELECT * FROM logs WHERE service = 'set'",
	}
	for _, query := range cases {
		if _, _, err := normalizeDuckDBSQL("logs", query); err != nil {
			t.Fatalf("normalizeDuckDBSQL(%q) error = %v", query, err)
		}
	}
}

func TestNormalizeDuckDBSQLRejectsNonSelectAndDatasetShadowing(t *testing.T) {
	cases := []string{
		"DELETE FROM logs",
		"SELECT * FROM logs; DROP TABLE logs",
		"WITH logs AS (SELECT 1) SELECT * FROM logs",
		"SELECT * FROM logs WHERE service = 'a' -- ; DROP",
		"COPY (SELECT * FROM logs) TO '/tmp/x.parquet'",
	}
	for _, query := range cases {
		if _, _, err := normalizeDuckDBSQL("logs", query); err == nil {
			t.Fatalf("normalizeDuckDBSQL(%q) unexpectedly succeeded", query)
		}
	}
}

func TestNormalizeDuckDBSQLRejectsUnknownDataset(t *testing.T) {
	if _, _, err := normalizeDuckDBSQL("secrets", "SELECT 1"); err == nil {
		t.Fatal("unknown dataset unexpectedly succeeded")
	}
}
