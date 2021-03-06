package bridge

import (
	"context"
	"fmt"
	"math/big"

	"go.uber.org/zap"
	"heimdallr/internal/avalanche"
	"heimdallr/internal/tezos"
)

type Bridge struct {
	avalanche *avalanche.Avalanche
	tezos     *tezos.Tezos

	logger *zap.SugaredLogger
}

type Event interface {
	User() string
	Amount() *big.Int
	Destination() string
}

func New(avalanche *avalanche.Avalanche, tezos *tezos.Tezos, logger *zap.SugaredLogger) *Bridge {
	return &Bridge{
		avalanche: avalanche,
		tezos:     tezos,
		logger:    logger,
	}
}

func (b *Bridge) Run(ctx context.Context) error {
	avaSub, err := b.avalanche.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("subscribe avalanche: %w", err)
	}

	tzsSub, err := b.tezos.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("subscribe tezos: %w", err)
	}

	b.logger.Info("Heimdallr is watching")
	b.loop(ctx, avaSub, tzsSub)

	return nil
}

func (b *Bridge) loop(ctx context.Context, avaSub *avalanche.Subscription, tzsSub *tezos.Subscription) {
	atomic := NewAtomic(
		WithChecker(b.checkOperation),
	)

	for {
		select {
		// Break loop on interruption
		case <-ctx.Done():
			return

		// Handle events from chains and call another chain
		case event := <-avaSub.OnAVAXLocked():
			swap := atomic.NewOperation(
				WithName("swap AVAX to WAVAX"),
				OnPerform(b.mintWAVAX),
				OnRollback(b.unlockAVAX),
			)
			go swap.Run(ctx, event)
		case event := <-avaSub.OnUSDCLocked():
			swap := atomic.NewOperation(
				WithName("swap USDC to WUSDC"),
				OnPerform(b.mintWUSDC),
				OnRollback(b.unlockUSDC),
			)
			go swap.Run(ctx, event)
		case event := <-tzsSub.OnWAVAXBurned():
			swap := atomic.NewOperation(
				WithName("swap WAVAX to AVAX"),
				OnPerform(b.unlockAVAX),
				OnRollback(b.mintWAVAX),
			)
			go swap.Run(ctx, event)
		case event := <-tzsSub.OnWUSDCBurned():
			swap := atomic.NewOperation(
				WithName("swap WUSDC to USDC"),
				OnPerform(b.unlockUSDC),
				OnRollback(b.mintWUSDC),
			)
			go swap.Run(ctx, event)

		// Handle errors occurred during chains subscriptions
		case err := <-avaSub.Err():
			b.logger.Errorf("avalanche subscribtion error: %s", err)
		case err := <-tzsSub.Err():
			b.logger.Errorf("tezos subscribtion error: %s", err)
		}
	}
}

func (b *Bridge) mintWAVAX(ctx context.Context, event Event) bool {
	hash, fee, err := b.tezos.MintWAVAX(ctx, event.Amount())
	if err != nil {
		b.logger.Errorf("mint wavax: %s", err)

		return false
	}

	b.logger.With(
		zap.String("user", event.User()),
		zap.Int64("amount", event.Amount().Int64()),
		zap.String("destination", event.Destination()),
		zap.String("tx_hash", hash),
		zap.Int64("fee", fee.Int64()),
	).Info("wavax minted")

	hash, fee, err = b.tezos.TransferWAVAX(ctx, event.Destination(), event.Amount())
	if err != nil {
		b.logger.Errorf("mint wavax: %s", err)

		return false
	}

	b.logger.With(
		zap.String("user", event.User()),
		zap.Int64("amount", event.Amount().Int64()),
		zap.String("destination", event.Destination()),
		zap.String("tx_hash", hash),
		zap.Int64("fee", fee.Int64()),
	).Info("wavax transferred")

	return true
}

func (b *Bridge) mintWUSDC(ctx context.Context, event Event) bool {
	hash, fee, err := b.tezos.MintWUSDC(ctx, event.Amount())
	if err != nil {
		b.logger.Errorf("mint wusdc: %s", err)

		return false
	}

	b.logger.With(
		zap.String("user", event.User()),
		zap.Int64("amount", event.Amount().Int64()),
		zap.String("destination", event.Destination()),
		zap.String("tx_hash", hash),
		zap.Int64("fee", fee.Int64()),
	).Info("wusdc minted")

	hash, fee, err = b.tezos.TransferWUSDC(ctx, event.Destination(), event.Amount())
	if err != nil {
		b.logger.Errorf("transfer wusdc: %s", err)

		return false
	}

	b.logger.With(
		zap.String("user", event.User()),
		zap.Int64("amount", event.Amount().Int64()),
		zap.String("destination", event.Destination()),
		zap.String("tx_hash", hash),
		zap.Int64("fee", fee.Int64()),
	).Info("wusdc transferred")

	return true
}

func (b *Bridge) unlockAVAX(ctx context.Context, event Event) bool {
	hash, fee, err := b.avalanche.UnlockAVAX(ctx, event.Destination(), event.Amount())
	if err != nil {
		b.logger.Errorf("unlock avax: %s", err)

		return false
	}

	b.logger.With(
		zap.String("user", event.User()),
		zap.Int64("amount", event.Amount().Int64()),
		zap.String("destination", event.Destination()),
		zap.String("tx_hash", hash),
		zap.Int64("fee", fee.Int64()),
	).Info("avax unlocked")

	return true
}

func (b *Bridge) unlockUSDC(ctx context.Context, event Event) bool {
	hash, fee, err := b.avalanche.UnlockUSDC(ctx, event.Destination(), event.Amount())
	if err != nil {
		b.logger.Errorf("unlock usdc: %s", err)

		return false
	}

	b.logger.With(
		zap.String("user", event.User()),
		zap.Int64("amount", event.Amount().Int64()),
		zap.String("destination", event.Destination()),
		zap.String("tx_hash", hash),
		zap.Int64("fee", fee.Int64()),
	).Info("usdc unlocked")

	return true
}
func (b *Bridge) checkOperation(op Checker, event Event) {
	select {
	case <-op.Complete():
		b.logger.With(
			zap.String("from", event.User()),
			zap.String("to", event.Destination()),
			zap.Int64("amount", event.Amount().Int64()),
		).Info("swap complete")
	case <-op.Rollback():
		b.logger.With(
			zap.String("from", event.User()),
			zap.String("to", event.Destination()),
			zap.Int64("amount", event.Amount().Int64()),
		).Info("swap rolled back")
	// Should not happen ever, because operation failing leads to coins lost.
	// Only contract owner will be able to unlock or mint lost coins.
	case err := <-op.Fail():
		b.logger.With(
			zap.String("from", event.User()),
			zap.String("to", event.Destination()),
			zap.Int64("amount", event.Amount().Int64()),
			zap.Error(err),
		).Debug("swap failed")
	}
}
