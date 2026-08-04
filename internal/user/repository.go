package user

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

type column string

const (
	colUsername column = "username"
	colEmail    column = "email"
)

var ErrEmailTaken = errors.New("email already taken")
var ErrUsernameTaken = errors.New("username already taken")
var ErrNotFound = errors.New("record not found")

type GormRepository struct {
	db *gorm.DB
}

func NewRepository(db *gorm.DB) *GormRepository {
	return &GormRepository{db: db}
}

func (r *GormRepository) Create(ctx context.Context, u *User) error {
	if err := r.db.WithContext(ctx).Create(u).Error; err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			switch pgErr.ConstraintName {
			case "uq_users_email":
				return ErrEmailTaken
			case "uq_users_username":
				return ErrUsernameTaken
			}
		}
		return err
	}
	return nil
}

func (r *GormRepository) FindByUsername(ctx context.Context, username string) (*User, error) {
	return r.findOne(ctx, "username", username)
}

func (r *GormRepository) FindByEmail(ctx context.Context, email string) (*User, error) {
	return r.findOne(ctx, "email", email)
}

func (r *GormRepository) findOne(ctx context.Context, col column, value string) (*User, error) {
	var u User
	if err := r.db.WithContext(ctx).Where(col+" = ?", value).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}
