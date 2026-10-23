// The standalone helper deliberately does not import internal/app.
package main

import (
	"os"

	"github.com/Mag1cFall/AIStudio2API/internal/authhelper"
)

func main() { os.Exit(authhelper.RunCLI(os.Args[1:], os.Stdout, os.Stderr)) }
