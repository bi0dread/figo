package adapters

import (
	"encoding/json"
	"testing"

	figo "github.com/bi0dread/figo/v4"
	"go.mongodb.org/mongo-driver/bson"
)

// The aggregation pipeline must be byte-identical across builds of the same
// state. Ranging the preload map emitted the $lookup (and its $match) stages
// in a different order every time, so the same query produced a different
// pipeline document run to run — defeating golden tests, plan caching and any
// diffing of generated queries. The raw adapter already sorts its JOINs.
func TestMongoAggregatePipelineIsDeterministic(t *testing.T) {
	const dsl = `load=[Orders:id=1|Items:id=2|Payments:id=3|Refunds:id=4]`
	// Every relation needs a MongoJoin: the old fallback invented a $lookup with
	// empty localField/foreignField that MongoDB rejects (error 40352), so the
	// builder now fails closed instead (hunt #6 H13).
	joins := map[string]MongoJoin{
		"Orders":   {From: "Orders", LocalField: "_id", ForeignField: "order_id", As: "Orders"},
		"Items":    {From: "Items", LocalField: "_id", ForeignField: "item_id", As: "Items"},
		"Payments": {From: "Payments", LocalField: "_id", ForeignField: "payment_id", As: "Payments"},
		"Refunds":  {From: "Refunds", LocalField: "_id", ForeignField: "refund_id", As: "Refunds"},
	}

	var first string
	for i := 0; i < 50; i++ {
		f := figo.New()
		if err := f.AddFiltersFromString(dsl); err != nil {
			t.Fatalf("AddFiltersFromString: %v", err)
		}
		f.Build(MongoAdapter{})

		pipeline, err := BuildMongoAggregatePipeline(f, joins)
		if err != nil {
			t.Fatalf("BuildMongoAggregatePipeline: %v", err)
		}
		encoded, err := json.Marshal(pipeline)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if i == 0 {
			first = string(encoded)
			continue
		}
		if string(encoded) != first {
			t.Fatalf("pipeline differs between builds:\n first: %s\n build %d: %s", first, i, encoded)
		}
	}
}

// Each $lookup is still immediately followed by its own $match.
func TestMongoAggregatePipelineLookupsSortedByRelation(t *testing.T) {
	f := figo.New()
	if err := f.AddFiltersFromString(`load=[Zebras:id=1|Apples:id=2]`); err != nil {
		t.Fatalf("AddFiltersFromString: %v", err)
	}
	f.Build(MongoAdapter{})

	joins := map[string]MongoJoin{
		"Zebras": {From: "Zebras", LocalField: "_id", ForeignField: "zebra_id", As: "Zebras"},
		"Apples": {From: "Apples", LocalField: "_id", ForeignField: "apple_id", As: "Apples"},
	}
	pipeline, err := BuildMongoAggregatePipeline(f, joins)
	if err != nil {
		t.Fatalf("BuildMongoAggregatePipeline: %v", err)
	}

	var order []string
	for _, stage := range pipeline {
		for _, elem := range stage {
			if elem.Key != "$lookup" {
				continue
			}
			doc, ok := elem.Value.(bson.D)
			if !ok {
				t.Fatalf("$lookup value should be a bson.D, got %T", elem.Value)
			}
			if from, ok := doc.Map()["from"].(string); ok {
				order = append(order, from)
			}
		}
	}
	if len(order) != 2 {
		t.Fatalf("want 2 $lookup stages, got %d (%v)", len(order), order)
	}
	if order[0] != "Apples" || order[1] != "Zebras" {
		t.Fatalf("want relations in sorted order [Apples Zebras], got %v", order)
	}
}
