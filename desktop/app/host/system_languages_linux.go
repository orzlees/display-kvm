package host

import "os"

func platformLanguages() ([]string, error) { return environmentLanguages(os.Getenv) }
