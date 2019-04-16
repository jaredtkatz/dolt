package tabular

import (
	"github.com/attic-labs/noms/go/types"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/row"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/schema"
	"github.com/liquidata-inc/ld/dolt/go/libraries/doltcore/table/untyped"
	"github.com/stretchr/testify/assert"
	"strings"
	"testing"
)

const (
	nameColName  = "name"
	ageColName   = "age"
	titleColName = "title"
	nameColTag   = 0
	ageColTag    = 1
	titleColTag  = 2
)

type StringBuilderCloser struct {
	strings.Builder
}
func (*StringBuilderCloser) Close() error {
	return nil
}

func TestWriter(t *testing.T) {

	var inCols = []schema.Column{
		{nameColName, nameColTag, types.StringKind, false, nil},
		{ageColName, ageColTag, types.StringKind, false, nil},
		{titleColName, titleColTag, types.StringKind, false, nil},
	}
	colColl, _ := schema.NewColCollection(inCols...)
	rowSch := schema.UnkeyedSchemaFromCols(colColl)

	// Simulate fixed-width string values that the table writer needs to function.
	// First value in each array is the column name
	names := []string {
		"name          ",
		"Michael Scott ",
		"Pam Beasley   ",
		"Dwight Schrute",
	}
	ages := []string {
		"age",
		"43 ",
		"25 ",
		"29 ",
	}
	titles := []string {
		"title                            ",
		"Regional Manager                 ",
		"Secretary                        ",
		"Assistant to the Regional Manager",
	}

	rows := make([]row.Row, len(names))
	for i := range names {
		rows[i] = row.New(rowSch, row.TaggedValues{
			nameColTag: types.String(names[i]),
			ageColTag: types.String(ages[i]),
			titleColTag: types.String(titles[i]),
		})
	}

	_, outSch := untyped.NewUntypedSchema(nameColName, ageColName, titleColName)

	var stringWr StringBuilderCloser
	tableWr := NewTextTableWriter(&stringWr, outSch)

	var expectedTableString = `
+----------------+-----+-----------------------------------+
| name           | age | title                             |
+----------------+-----+-----------------------------------+
| Michael Scott  | 43  | Regional Manager                  |
| Pam Beasley    | 25  | Secretary                         |
| Dwight Schrute | 29  | Assistant to the Regional Manager |
+----------------+-----+-----------------------------------+
`
	// strip off the first newline, inserted for nice printing
	expectedTableString = strings.Replace(expectedTableString, "\n", "", 1)

	for _, r := range rows {
		tableWr.WriteRow(r)
	}
	tableWr.Close()

	assert.Equal(t, expectedTableString, stringWr.String())
}

