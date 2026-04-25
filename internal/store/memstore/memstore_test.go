package memstore_test

import (
	"testing"

	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/memstore"
	"github.com/itsHabib/orchestra/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.RunConformance(t, func(*testing.T) store.Store {
		return memstore.New()
	})
}
