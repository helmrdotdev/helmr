package computerhost

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
