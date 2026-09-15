package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kpenfound/busybees/internal/mcpserver"
)

// These wire and CLI fingerprints pin the complete pre-extraction public
// contract, including descriptions, annotations, schemas, ordering and enums.
func TestMCPPublicContract(t *testing.T) {
	want := map[string][2]string{
		"":                {"339f9396e0aa0605800402ea8a44631d6f68f7f5bb0590d2605f6fe9fe1cbff0", "d371d2547e337236577d250b793cf53f2f4042f4bd533fba315c1af81b256f49"},
		"unknown":         {"339f9396e0aa0605800402ea8a44631d6f68f7f5bb0590d2605f6fe9fe1cbff0", "d371d2547e337236577d250b793cf53f2f4042f4bd533fba315c1af81b256f49"},
		"developer":       {"3a4fbac9ee8bc38edf939cfa1d6005fe5dd567d129c7c6191ecfbf8cf03da027", "cd0cf944dd9a1cc687acc2437786807d36e5fb53851c33ea29e0456a0995dc4d"},
		"reviewer":        {"5ab0d26ddce78d7a5ae162602c02984673f79cb81f258776872e7d2684b687c6", "4a619b8d8fc571ba4507317ca8a257a2eac1ad6ca26450f670a8d5da9de0fb2a"},
		"qa":              {"5194318ad062acf8c645ca511190aee8b88766c8d7ab14e90fa980c852190cad", "9ceebcfcd1b924c7211ba295a686ff14fc33d9fbdbd034de6d77c8c3c9ed515c"},
		"product_manager": {"f759645179fa1861ca358d93fb5a88c6ce51f89f6405a7e61f39b7298e9d7e7a", "8142d0707976064e4559242c18a7bb146fcdc952f43e725839e63d0fa67489fb"},
		"project_manager": {"4adedefb4a4907947a9735ef874ad0873a0919b0435a817280520e031bee5d66", "6f66c5023af5758351b60735aadb88a112b4c0f9caad67c5970e682dceb64340"},
	}
	for role, hashes := range want {
		list, err := mcpserver.Tools(context.Background(), mcpserver.Env{Role: role})
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(list)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != hashes[0] {
			t.Errorf("role %q wire contract changed (%s): %s", role, got, data)
		}
		output := toolsText(list)
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(output))); got != hashes[1] {
			t.Errorf("role %q CLI contract changed (%s):\n%s", role, got, output)
		}
	}
}
