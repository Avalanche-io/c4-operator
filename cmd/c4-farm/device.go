package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/Avalanche-io/c4-operator/internal/farmcli"
	"github.com/Avalanche-io/c4-operator/internal/keystore"
)

// deviceCommand is `c4 farm device approve DEVICE`: the person approves
// another machine's key to enrol as them, with their touch (PEOPLE-KEYS-2
// 7). Only a person at a terminal approves; never an agent.
func deviceCommand(ctx context.Context, fs *flag.FlagSet, args []string, stdout io.Writer, terminal bool, keys keystore.Store, connect connectFunc) error {
	const use = "usage: c4 farm device approve DEVICE (the device ID the other machine shows, c4…)"
	if len(args) == 0 || args[0] != "approve" {
		return errors.New(use)
	}
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New(use)
	}
	switch {
	case farmcli.InsideAgent():
		return errors.New("an agent never approves a machine: approve it yourself, at a terminal")
	case !terminal:
		return errors.New("approving a machine takes your touch: run it yourself, at a terminal")
	}
	c, _, _, _, err := connect()
	if err != nil {
		return err
	}
	cfg, err := farmcli.PeopleConfig(ctx, c)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Approve only if the other screen shows: %s\n", farmcli.ShortDevice(pos[0]))
	fmt.Fprintf(stdout, "Your Mac asks for your touch next; cancel it if the screens don't match.\n")
	if _, err := farmcli.ApproveDevice(ctx, c, keys, cfg.Farm, pos[0]); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "approved  the other machine enrols as you within 10 minutes; c4 farm tells you here once it has\n")
	return nil
}
