package memstore_test

import (
	"testing"

	"github.com/itsHabib/orchestra/pkg/store"
	"github.com/itsHabib/orchestra/pkg/store/memstore"
	"github.com/itsHabib/orchestra/pkg/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.RunConformance(t, func(*testing.T) store.Store {
		return memstore.New()
	})
}
