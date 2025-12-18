package main

import (
	"fmt"
	"github.com/neovim/go-client/nvim"
)

type Vim struct {
	api *nvim.Nvim
}

type selectOpts struct {
	Title string `msgpack:"title"`
}

// select displays a selection menu and returns the selected indexprompt
func (vim Vim) uiSelect(items []string, opts selectOpts) (int, error) {
	promptLines := []string{opts.Title}
	for i, item := range items {
		promptLines = append(promptLines, fmt.Sprintf("%d. %s", i+1, item))
	}

	var choice int
	err := vim.api.Call("inputlist", &choice, promptLines)
	if err != nil {
		return -1, fmt.Errorf("error calling inputlist: %w", err)
	}

	// choice is 1-indexed, 0 means cancelled or invalid
	if choice < 1 || choice > len(items) {
		return -1, nil
	}

	return choice, nil
}

func (vim Vim) bufnr(name string, create bool) (nvim.Buffer, error) {
	var result int
	err := vim.api.Call("bufnr", &result, []byte(name), create)
	// Handle if result is falsy (0) because of error in ACP client
	if result == 0 {
		result = 1
	}
	return nvim.Buffer(result), err
}

func starString(s string) *string {
	return &s
}
