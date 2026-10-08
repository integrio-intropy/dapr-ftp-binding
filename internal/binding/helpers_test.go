package binding

import (
	contribmd "github.com/dapr/components-contrib/metadata"
)

func metadataBase(props map[string]string) contribmd.Base {
	return contribmd.Base{Properties: props}
}
