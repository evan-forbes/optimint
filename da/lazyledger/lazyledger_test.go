package lazyledger

import (
	"errors"
	"testing"

	"github.com/cosmos/cosmos-sdk/crypto/hd"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/lazyledger/optimint/da"
	"github.com/lazyledger/optimint/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfiguration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		input    []byte
		err      error
		expected Config
	}{
		{"empty config", []byte(""), errors.New("unknown keyring backend "), Config{}},
		{"with namespace id", []byte("NamespaceID = [3, 2, 1]\nBackend = 'test'"), nil, Config{NamespaceID: []byte{0x03, 0x02, 0x01}, Backend: "test"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert := assert.New(t)
			ll := &LazyLedger{}
			err := ll.Init(c.input, nil)

			if c.err != nil {
				assert.EqualError(err, c.err.Error())
			} else {
				assert.NoError(err)
				assert.Equal(c.expected, ll.config)
			}

		})
	}
}

func TestSubmission(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	block := &types.Block{Header: types.Header{
		Height: 1,
	}}

	ll := &LazyLedger{}
	err := ll.Init([]byte("From='test'\nNamespaceID = [3, 2, 1, 0, 3, 2, 1, 0]\nBackend = 'test'"), nil)
	ll.keyring = generateKeyring(t, "test")
	require.NoError(err)
	result := ll.SubmitBlock(block)
	assert.Equal(da.StatusSuccess, result.Code)
	assert.Equal("", result.Message)
}

func generateKeyring(t *testing.T, accts ...string) keyring.Keyring {
	kb := keyring.NewInMemory()

	for _, acc := range accts {
		_, _, err := kb.NewMnemonic(acc, keyring.English, "", hd.Secp256k1)
		if err != nil {
			t.Error(err)
		}
	}

	return kb
}