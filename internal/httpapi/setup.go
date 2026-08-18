package httpapi

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"aihub/internal/authn"
	"aihub/internal/model"
	"aihub/internal/store"
)

// setup creates the very first administrator from the browser, which is what a
// fresh deployment shows instead of a sign-in form. It is the only
// unauthenticated endpoint that writes, so it is guarded twice: it refuses once
// any account exists, and the INSERT carries the same condition (see
// store.CreateFirstAdmin) so two people opening the page at the same time cannot
// both become the owner.
//
// The window between the first start and a completed setup is open to whoever
// reaches the port first, exactly like every other install wizard. Deployments
// that cannot accept that should set AIHUB_ADMIN_USERNAME and
// AIHUB_ADMIN_PASSWORD instead, which closes the window before the server ever
// listens.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) *apiError {
	var body struct {
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if apiErr := decodeJSON(r, &body); apiErr != nil {
		return apiErr
	}

	// Checked before the password is hashed, so a configured deployment answers
	// this immediately instead of paying for a bcrypt round it will discard.
	count, err := s.store.CountUsers(r.Context())
	if err != nil {
		return storeError(err, "count accounts")
	}
	if count > 0 {
		return setupDoneError()
	}

	username := trim(body.Username)
	if !model.ValidUsername(username) {
		return invalid(map[string]string{"username": usernameRequirement})
	}
	// There is no generated-password branch here, unlike the admin create-user
	// form: nobody is signed in yet to be shown the result.
	hash, err := authn.HashPassword(body.Password)
	if err != nil {
		return invalid(map[string]string{"password": err.Error()})
	}

	user := &model.User{
		Username:     username,
		DisplayName:  firstNonEmpty(trim(body.DisplayName), username),
		Role:         model.RoleAdmin,
		Status:       model.StatusActive,
		PasswordHash: hash,
	}
	if err = s.store.CreateFirstAdmin(r.Context(), user, model.UnlimitedQuota(uuid.Nil)); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return setupDoneError()
		}
		return storeError(err, "create the first administrator")
	}
	s.log.Info("first administrator created through setup", "username", user.Username, "id", user.ID)

	// Signing the operator in here is the whole point of doing this in the UI:
	// no password to copy out of a log, no second trip through the login form.
	resp, apiErr := s.issueSession(w, r, user)
	if apiErr != nil {
		return apiErr
	}
	if err = s.store.TouchUserLogin(r.Context(), user.ID); err != nil {
		s.log.Warn("could not record login time", "user", user.ID, "error", err)
	}

	writeJSON(w, http.StatusCreated, resp)
	return nil
}

// setupDoneError is returned by both guards so the UI sees one code to branch on
// when it should send the visitor to the sign-in form instead.
func setupDoneError() *apiError {
	return errorf(http.StatusConflict, "setup_complete",
		"this deployment already has an account; sign in instead")
}

// needsSetup reports whether the deployment still has no accounts at all. A
// database error is reported as "configured": showing the setup form because a
// query timed out would invite somebody to try to create a second owner.
func (s *Server) needsSetup(r *http.Request) bool {
	count, err := s.store.CountUsers(r.Context())
	if err != nil {
		s.log.Warn("could not count accounts for the setup check", "error", err)
		return false
	}
	return count == 0
}
