package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// confirmer asks the operator before a destructive step. With assumeYes the answer is always yes,
// which is what scripts pass as --yes.
func confirmer(in io.Reader, out io.Writer, assumeYes bool) func(prompt string) (bool, error) {
	reader := bufio.NewReader(in)
	return func(prompt string) (bool, error) {
		if assumeYes {
			return true, nil
		}
		fmt.Fprintf(out, "%s Type 'yes' to continue: ", prompt)
		line, err := reader.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			return false, errors.New("no answer could be read from the terminal: run again with --yes to confirm in a script")
		}
		return strings.EqualFold(strings.TrimSpace(line), "yes"), nil
	}
}
