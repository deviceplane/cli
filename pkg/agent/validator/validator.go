package validator

import (
	"github.com/deviceplane/cli/pkg/models"
)

type Validator interface {
	Validate(models.Service) error
	Name() string
}
