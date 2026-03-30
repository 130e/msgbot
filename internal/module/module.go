package module

import "context"

type Module interface {
	Up(context.Context) error
	Down(context.Context) error
}
