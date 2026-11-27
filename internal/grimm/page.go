package grimm

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// The page's writes: each runs in its own transaction under the session's lock, as the human.

func (s *Service) inSession(ctx context.Context, account int64, id string, fn func(tx pgx.Tx, sess Session) error) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		sess, err := activeSession(ctx, tx, account, id)
		if err != nil {
			return err
		}
		return fn(tx, sess)
	})
}

// HireWorker hires a worker the human asked for.
func (s *Service) HireWorker(ctx context.Context, account int64, id string, h Hire) (Worker, error) {
	var w Worker
	err := s.inSession(ctx, account, id, func(tx pgx.Tx, sess Session) error {
		var err error
		w, _, err = s.hire(ctx, tx, sess, h, "human")
		return err
	})
	return w, err
}

// SayToWorker sends the human's message straight into a worker's session.
func (s *Service) SayToWorker(ctx context.Context, account int64, id, name, text string) (string, error) {
	var out string
	err := s.inSession(ctx, account, id, func(tx pgx.Tx, sess Session) error {
		var err error
		out, err = s.say(ctx, tx, sess, "human", name, text)
		return err
	})
	return out, err
}

// StopWorker interrupts a worker's turn.
func (s *Service) StopWorker(ctx context.Context, account int64, id, name string) (string, error) {
	var out string
	err := s.inSession(ctx, account, id, func(tx pgx.Tx, sess Session) error {
		var err error
		out, err = s.stop(ctx, tx, sess, "human", name)
		return err
	})
	return out, err
}

// ArchiveWorkers archives one worker, or with name "" all of them.
func (s *Service) ArchiveWorkers(ctx context.Context, account int64, id, name string) ([]string, error) {
	var out []string
	err := s.inSession(ctx, account, id, func(tx pgx.Tx, sess Session) error {
		var err error
		out, err = s.archiveWorkers(ctx, tx, sess, name, "human")
		return err
	})
	if out == nil {
		out = []string{}
	}
	return out, err
}

// AddDirective adds a standing rule and pushes it to busy workers; it returns its number and who got it.
func (s *Service) AddDirective(ctx context.Context, account int64, id, text string) (int, []string, error) {
	var seq int
	var pushed []string
	err := s.inSession(ctx, account, id, func(tx pgx.Tx, sess Session) error {
		var err error
		seq, pushed, err = s.addDirective(ctx, tx, sess, "human", text)
		return err
	})
	if pushed == nil {
		pushed = []string{}
	}
	return seq, pushed, err
}

// RemoveDirective retires standing rule seq: kept with removed_at for the record, no longer sent to new agents.
func (s *Service) RemoveDirective(ctx context.Context, account int64, id string, seq int) error {
	return s.inSession(ctx, account, id, func(tx pgx.Tx, sess Session) error {
		_, err := s.removeDirective(ctx, tx, sess, "human", seq)
		return err
	})
}
