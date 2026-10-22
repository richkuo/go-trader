package notifybroker

import (
	"context"
	"fmt"

	"trading-scheduler/internal/egresspolicy"
)

func Run(ctx context.Context, deployment string) error {
	a, err := egresspolicy.Attest(ctx, deployment, egresspolicy.RoleNotify, egresspolicy.Identity{})
	if err != nil {
		return err
	}
	ep, err := egresspolicy.ParseProxyURL("http://" + a.Notify.ProxyAddress)
	if err != nil {
		return fmt.Errorf("notification proxy address: %w", err)
	}
	b := NewBrokerFromEnv(a.WorkerUID, a.Notify.Socket, egresspolicy.NewDialer(ep), a.Notify.Destinations)
	return b.Run(ctx)
}
