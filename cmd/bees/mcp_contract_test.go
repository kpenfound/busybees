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
		"":                {"546717fa9e544c7b96f0deb90c0b98a3821cfce3bc5b61cdad0e545a69e7f3df", "ce88d26d56d9252cd5229b7b5a9d5adc3a8996159bd1964fcb9162329e0f6c51"},
		"unknown":         {"546717fa9e544c7b96f0deb90c0b98a3821cfce3bc5b61cdad0e545a69e7f3df", "ce88d26d56d9252cd5229b7b5a9d5adc3a8996159bd1964fcb9162329e0f6c51"},
		"developer":       {"190d48cc1c62983a5a9d57abb1c04495d4668235b3e745bf3c88b61bc7ea2979", "c7715603e82165858639ff052b3823c65e8536a46b84c624c3ba00ab8ed11b9c"},
		"reviewer":        {"f08b5c88486429809e60354d4ece28e8c4827a96d21ed42f09a42818d6f039de", "95900dc52d4b37444c568041c5220b23982aa19adfeb58db13519ce96c39995c"},
		"qa":              {"1fc52836772d1e22abcc552881d0e174022348230a5b951e498ca674db8c5af5", "28659f6a998183106f307a7f50d49ce1a282fbaeb694693e80108e70347b00cb"},
		"product_manager": {"4879463171ce997da0f44cd90a08889417af9f8daabfef5c891f323c1d6cf92f", "5e224265e6d817308a9594d4e7fe6e68554c3f5201e9d6f0ae62271c72d915a1"},
		"project_manager": {"df1620c67c17a2c0f331fee829157877042fe97ddb31b2b89b5ee66dd114bf43", "14caf4b43e71e4550c816cbfd18b946698840f5c87eab20b246ff27b199327db"},
		"release_manager": {"bae42720ae739b92c283e11aee97894c01a586e93e232080ed37c0dbc7662670", "5d3b7ef51d86e37e6d7e407aa683fa18a31db3c593398ce9e9f7a60de4cdd8fb"},
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
