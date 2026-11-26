package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"enterprise-im/backend/internal/model"
)

var (
	// ErrAuthUserNotFound indicates the target user row does not exist.
	ErrAuthUserNotFound = errors.New("auth user not found")
	// ErrDuplicateUsername indicates uk_users_username uniqueness violation.
	ErrDuplicateUsername = errors.New("username already exists")
)

// AuthRepository encapsulates user credential queries and auth transactions.
type AuthRepository struct {
	db *gorm.DB
}

// NewAuthRepository creates an AuthRepository backed by db.
func NewAuthRepository(db *gorm.DB) *AuthRepository {
	return &AuthRepository{db: db}
}

// WithTx executes fn inside a MySQL transaction.
func (r *AuthRepository) WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return WithTx(ctx, r.db, fn)
}

// CreateUser inserts a new user record, translating duplicate key errors to ErrDuplicateUsername.
func (r *AuthRepository) CreateUser(ctx context.Context, user *model.User) error {
	if r == nil || r.db == nil {
		return errors.New("auth repository db is nil")
	}
	if err := r.db.WithContext(ctx).Create(user).Error; err != nil {
		if IsMySQLDuplicateError(err) {
			return ErrDuplicateUsername
		}
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

// FindUserByUsername retrieves a user record by exact username.
func (r *AuthRepository) FindUserByUsername(ctx context.Context, username string) (*model.User, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("auth repository db is nil")
	}
	var user model.User
	err := r.db.WithContext(ctx).Where("username = ?", username).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAuthUserNotFound
		}
		return nil, fmt.Errorf("find user by username: %w", err)
	}
	return &user, nil
}

// FindUserByID retrieves a user record by primary key id.
func (r *AuthRepository) FindUserByID(ctx context.Context, userID uint64) (*model.User, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("auth repository db is nil")
	}
	var user model.User
	err := r.db.WithContext(ctx).Where("id = ?", userID).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAuthUserNotFound
		}
		return nil, fmt.Errorf("find user by id: %w", err)
	}
	return &user, nil
}

// FindUserByIDForUpdate locks and loads a user row inside an active transaction.
func (r *AuthRepository) FindUserByIDForUpdate(ctx context.Context, tx *gorm.DB, userID uint64) (*model.User, error) {
	if tx == nil {
		return nil, errors.New("transaction handle is nil")
	}
	var user model.User
	err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", userID).
		First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAuthUserNotFound
		}
		return nil, fmt.Errorf("lock user by id: %w", err)
	}
	return &user, nil
}

// UpdatePasswordHash updates password_hash for userID within tx.
func (r *AuthRepository) UpdatePasswordHash(ctx context.Context, tx *gorm.DB, userID uint64, passwordHash string) error {
	if tx == nil {
		return errors.New("transaction handle is nil")
	}
	res := tx.WithContext(ctx).
		Model(&model.User{}).
		Where("id = ?", userID).
		Update("password_hash", passwordHash)
	if res.Error != nil {
		return fmt.Errorf("update user password hash: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrAuthUserNotFound
	}
	return nil
}

// GetUserAuthStatus returns the user's current status and role for middleware verification.
func (r *AuthRepository) GetUserAuthStatus(ctx context.Context, userID uint64) (status int8, role string, err error) {
	if r == nil || r.db == nil {
		return 0, "", errors.New("auth repository db is nil")
	}
	var row struct {
		Status int8   `gorm:"column:status"`
		Role   string `gorm:"column:role"`
	}
	err = r.db.WithContext(ctx).
		Model(&model.User{}).
		Select("status", "role").
		Where("id = ?", userID).
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, "", ErrAuthUserNotFound
		}
		return 0, "", fmt.Errorf("query user auth status: %w", err)
	}
	return row.Status, row.Role, nil
}

// IsMySQLDuplicateError reports whether err represents a unique key conflict.
func IsMySQLDuplicateError(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	var mysqlErr *mysqlDriver.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1062
	}
	return false
}

// IsDBUnavailableError reports whether err looks like a MySQL connectivity/timeout error.
func IsDBUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var mysqlErr *mysqlDriver.MySQLError
	if errors.As(err, &mysqlErr) {
		return false
	}
	return errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.EOF)
}
