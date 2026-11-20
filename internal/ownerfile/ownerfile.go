package ownerfile

import "os"

// Write creates or truncates path with owner-only access, restricted before
// the data is written.
func Write(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err = RestrictToUser(path); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
