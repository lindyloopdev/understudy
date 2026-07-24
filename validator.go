package understudy

import "context"

type validatorFunc func(ctx context.Context, token string) (*BackendConfig, error)

func (f validatorFunc) Validate(ctx context.Context, token string) (*BackendConfig, error) {
	return f(ctx, token)
}

// SingleToken returns a [TokenValidator] that resolves a non-empty token equal
// to token to backend, and returns [ErrInvalidToken] for every other token.
// Unlike [StaticToken] it has no fallthrough; it is the terminal validator in a
// chain.
func SingleToken(token string, backend *BackendConfig) TokenValidator {
	return validatorFunc(func(_ context.Context, t string) (*BackendConfig, error) {
		if token != "" && t == token {
			return backend, nil
		}
		return nil, ErrInvalidToken
	})
}

// StaticToken returns a [TokenValidator] that resolves a non-empty token equal
// to token to backend, delegating every other token to next. An empty token
// never matches, so a permanent token is honored only when one is explicitly
// configured.
func StaticToken(token string, backend *BackendConfig, next TokenValidator) TokenValidator {
	return validatorFunc(func(ctx context.Context, t string) (*BackendConfig, error) {
		if token != "" && t == token {
			return backend, nil
		}
		return next.Validate(ctx, t)
	})
}

// Validator builds the token validator for the configuration: the permanent
// token (if any) resolves to backend, and every other token falls through to
// next.
func (c Config) Validator(backend *BackendConfig, next TokenValidator) TokenValidator {
	return StaticToken(c.Token, backend, next)
}
