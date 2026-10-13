package auth

import (
	"errors"
	"io/fs"

	"github.com/NeuralTrust/ntctl/internal/config"
)

// ErrNotLoggedIn means there are no saved credentials for the console.
var ErrNotLoggedIn = errors.New("not signed in; run `ntctl login`")

// Credentials are the saved tokens for one console.
type Credentials struct {
	Token
	Email string `json:"email,omitempty"`
}

// credentialsFile holds the credentials of every console signed in to, keyed
// by console URL. It is readable only by the user.
const credentialsFile = "credentials.json"

func loadAll() (map[string]*Credentials, error) {
	all := map[string]*Credentials{}
	if err := config.ReadJSON(credentialsFile, &all); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return all, nil
}

// Load returns the saved credentials for a console.
func Load(appURL string) (*Credentials, error) {
	all, err := loadAll()
	if err != nil {
		return nil, err
	}
	c, ok := all[appURL]
	if !ok || c == nil || (c.AccessToken == "" && c.RefreshToken == "") {
		return nil, ErrNotLoggedIn
	}
	return c, nil
}

// Save stores the credentials for a console.
func Save(appURL string, c *Credentials) error {
	all, err := loadAll()
	if err != nil {
		return err
	}
	all[appURL] = c
	return config.WriteJSON(credentialsFile, all)
}

// Delete forgets the credentials for a console.
func Delete(appURL string) error {
	all, err := loadAll()
	if err != nil {
		return err
	}
	if _, ok := all[appURL]; !ok {
		return nil
	}
	delete(all, appURL)
	return config.WriteJSON(credentialsFile, all)
}
