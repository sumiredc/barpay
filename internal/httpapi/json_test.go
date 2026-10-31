package httpapi

import "testing"

// Keys inside objects in an array are checked like any other, and named with their index.
func TestCheckKeysLooksInsideArrays(t *testing.T) {
	allowed := Fields{"id": nil, "lines": Fields{"name": nil, "price": MoneyFields()}}
	cases := []struct{ body, field string }{
		{`{"id":"a","lines":[{"name":"x","price":{"value":1,"currency":"USD"}}]}`, ""},
		{`{"id":"a","lines":[{"name":"x"},{"Name":"y"}]}`, "lines[1].Name"},
		{`{"id":"a","lines":[{"name":"x","name":"y"}]}`, "lines[0].name"},
		{`{"id":"a","lines":[{"price":{"value":1,"Value":2}}]}`, "lines[0].price.Value"},
		{`{"id":"a","lines":[1,{"x":1}]}`, "lines[1].x"},
		{`{"id":"a","lines":[{"name":"x"}],"id":"b"}`, "id"},
		{`{"lines":[{"name":{"deep":[{"anything":1}]}}],"other":1}`, "other"},
	}
	for _, c := range cases {
		field, err := CheckKeys([]byte(c.body), allowed)
		if field != c.field || (err != nil) != (c.field != "") {
			t.Errorf("%s: field %q, err %v; want field %q", c.body, field, err, c.field)
		}
	}
}
