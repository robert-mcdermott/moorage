package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/names"
)

// The roles of a project's members.
const (
	RoleViewer     = "viewer"
	RoleDeployer   = "deployer"
	RoleMaintainer = "maintainer"
)

// DefaultProject is the project every app starts in, unless it says otherwise.
const (
	DefaultProject   = "default"
	defaultProjectID = "prj_default"
)

// ValidRole reports whether a name is a project's role.
func ValidRole(role string) bool {
	return role == RoleViewer || role == RoleDeployer || role == RoleMaintainer
}

// Project holds apps, whose members have roles in it.
type Project struct {
	ID, Name string
	// EveryoneRole is the role of those who are not members: "" for none.
	EveryoneRole string
	// ViewersSeeLogs says whether viewers may read logs.
	ViewersSeeLogs bool
	CreatedAt      time.Time
	// Apps counts its apps, where a list says so.
	Apps int
}

// Member is someone's role in a project.
type Member struct {
	UserID, Email, Name, Role string
}

// InvalidNameError says why something cannot be called that.
type InvalidNameError struct{ Err error }

func (e *InvalidNameError) Error() string { return e.Err.Error() }

// ErrProjectExists is returned for a name already taken.
var ErrProjectExists = errors.New("there is already a project of that name")

// ErrProjectNotEmpty is returned on deleting a project that has apps.
var ErrProjectNotEmpty = errors.New("the project still has apps: move or delete them first")

// ErrDefaultProject is returned on deleting the default project.
var ErrDefaultProject = errors.New("the default project cannot be deleted")

const projectColumns = `p.id, p.name, p.everyone_role, p.viewers_see_logs, p.created_at`

func scanProject(row interface{ Scan(...any) error }, extra ...any) (Project, error) {
	var p Project
	var everyone sql.NullString
	var logs int
	var created int64
	dest := append([]any{&p.ID, &p.Name, &everyone, &logs, &created}, extra...)
	if err := row.Scan(dest...); err != nil {
		return p, err
	}
	p.EveryoneRole, p.ViewersSeeLogs, p.CreatedAt = everyone.String, logs != 0, time.UnixMilli(created).UTC()
	return p, nil
}

// Projects lists the projects by name, default first, with how many apps each has.
func (db *DB) Projects(ctx context.Context) ([]Project, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+projectColumns+`, (SELECT count(*) FROM apps a WHERE a.project_id = p.id)
		FROM projects p ORDER BY p.name <> ?, p.name`, DefaultProject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var apps int
		p, err := scanProject(rows, &apps)
		if err != nil {
			return nil, err
		}
		p.Apps = apps
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectByName returns a project.
func (db *DB) ProjectByName(ctx context.Context, name string) (Project, error) {
	p, err := scanProject(db.Reader.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects p WHERE p.name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("no project %q: %w", name, ErrNotFound)
	}
	return p, err
}

// AppProject returns the project an app is in.
func (db *DB) AppProject(ctx context.Context, app string) (Project, error) {
	p, err := scanProject(db.Reader.QueryRowContext(ctx, `SELECT `+projectColumns+`
		FROM apps a JOIN projects p ON p.id = coalesce(a.project_id, ?) WHERE a.name = ?`, defaultProjectID, app))
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("no app %q: %w", app, ErrNotFound)
	}
	return p, err
}

// CreateProject makes a project, with nobody but its members and admins in it.
func (db *DB) CreateProject(ctx context.Context, name string) (Project, error) {
	if err := names.ValidateProject(name); err != nil {
		return Project{}, &InvalidNameError{err}
	}
	now := time.Now().UTC()
	p := Project{ID: ids.New(ids.Project), Name: name, ViewersSeeLogs: true, CreatedAt: now}
	err := db.write(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM projects WHERE name = ?`, name).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return ErrProjectExists
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO projects (id, name, created_at) VALUES (?, ?, ?)`, p.ID, name, now.UnixMilli())
		return err
	})
	return p, err
}

// SetProject changes who may do what in a project without being a member of it.
func (db *DB) SetProject(ctx context.Context, name, everyoneRole string, viewersSeeLogs bool) error {
	var everyone any
	if everyoneRole != "" {
		everyone = everyoneRole
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE projects SET everyone_role = ?, viewers_see_logs = ? WHERE name = ?`, everyone, boolInt(viewersSeeLogs), name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no project %q: %w", name, ErrNotFound)
		}
		return nil
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// DeleteProject removes a project that has no apps.
func (db *DB) DeleteProject(ctx context.Context, name string) error {
	if name == DefaultProject {
		return ErrDefaultProject
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = ?`, name).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no project %q: %w", name, ErrNotFound)
		}
		if err != nil {
			return err
		}
		var apps int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM apps WHERE project_id = ?`, id).Scan(&apps); err != nil {
			return err
		}
		if apps > 0 {
			return ErrProjectNotEmpty
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
		return err
	})
}

// Members lists a project's members.
func (db *DB) Members(ctx context.Context, project string) ([]Member, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT u.id, u.email, u.name, m.role
		FROM project_members m JOIN projects p ON p.id = m.project_id JOIN users u ON u.id = m.user_id
		WHERE p.name = ? ORDER BY u.email`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Email, &m.Name, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMember gives someone a role in a project.
func (db *DB) SetMember(ctx context.Context, project, userID, role string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = ?`, project).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no project %q: %w", project, ErrNotFound)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO project_members (project_id, user_id, role, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (project_id, user_id) DO UPDATE SET role = excluded.role`, id, userID, role, time.Now().UnixMilli())
		return err
	})
}

// LimitEveryone keeps someone from the roles that a project gives everyone, as a viewer of it. For an account that
// nobody chose to give: otherwise, whoever a provider signs in could deploy to the default project.
func (db *DB) LimitEveryone(ctx context.Context, userID string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO project_members (project_id, user_id, role, created_at)
			SELECT id, ?, 'viewer', ? FROM projects WHERE everyone_role IN ('deployer', 'maintainer')
			ON CONFLICT (project_id, user_id) DO NOTHING`, userID, time.Now().UnixMilli())
		return err
	})
}

// RemoveMember takes someone out of a project.
func (db *DB) RemoveMember(ctx context.Context, project, userID string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM project_members WHERE user_id = ?
			AND project_id = (SELECT id FROM projects WHERE name = ?)`, userID, project)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: they are not a member of %s", ErrNotFound, project)
		}
		return nil
	})
}

// MemberRole returns someone's own role in a project, if they have one.
func (db *DB) MemberRole(ctx context.Context, projectID, userID string) (string, error) {
	var role string
	err := db.Reader.QueryRowContext(ctx, `SELECT role FROM project_members WHERE project_id = ? AND user_id = ?`, projectID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// VisibleProjects returns the IDs of the projects someone has a role in, as a member or
// as anyone, with the role.
func (db *DB) VisibleProjects(ctx context.Context, userID string) (map[string]string, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT p.id, coalesce(m.role, p.everyone_role)
		FROM projects p LEFT JOIN project_members m ON m.project_id = p.id AND m.user_id = ?
		WHERE m.role IS NOT NULL OR p.everyone_role IS NOT NULL`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, role string
		if err := rows.Scan(&id, &role); err != nil {
			return nil, err
		}
		out[id] = role
	}
	return out, rows.Err()
}

// AppProjectIDs returns each app's project ID, by app name.
func (db *DB) AppProjectIDs(ctx context.Context) (map[string]string, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT name, coalesce(project_id, ?) FROM apps`, defaultProjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var app, project string
		if err := rows.Scan(&app, &project); err != nil {
			return nil, err
		}
		out[app] = project
	}
	return out, rows.Err()
}

// CreateApp makes an app in a project, with its production environment.
func (db *DB) CreateApp(ctx context.Context, name, project string) (string, bool, error) {
	if err := names.ValidateApp(name); err != nil {
		return "", false, err
	}
	var id string
	var created bool
	err := db.write(ctx, func(tx *sql.Tx) error {
		var projectID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = ?`, project).Scan(&projectID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no project %q: %w", project, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if id, created, err = ensureApp(ctx, tx, name, time.Now()); err != nil {
			return err
		}
		if created {
			_, err = tx.ExecContext(ctx, `UPDATE apps SET project_id = ? WHERE id = ?`, projectID, id)
		}
		return err
	})
	return id, created, err
}

// MoveApp puts an app in another project.
func (db *DB) MoveApp(ctx context.Context, app, project string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		var projectID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = ?`, project).Scan(&projectID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no project %q: %w", project, ErrNotFound)
		}
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE apps SET project_id = ? WHERE name = ?`, projectID, app)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("no app %q: %w", app, ErrNotFound)
		}
		return nil
	})
}

// DeleteApp removes an app, and with it its environments, variables, releases, builds and
// deployments. Events and the audit log keep what happened, by name.
func (db *DB) DeleteApp(ctx context.Context, name string) error {
	return db.write(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM apps WHERE name = ?`, name).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no app %q: %w", name, ErrNotFound)
		}
		if err != nil {
			return err
		}
		// The environment names its release, which names its environment: that way round
		// is the one a cascade cannot do.
		if _, err := tx.ExecContext(ctx, `UPDATE environments SET current_release_id = NULL WHERE app_id = ?`, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM apps WHERE id = ?`, id)
		return err
	})
}

// RenameApp gives an app another name, in everything that holds it.
func (db *DB) RenameApp(ctx context.Context, from, to string) error {
	if err := names.ValidateApp(to); err != nil {
		return err
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM apps WHERE name = ?`, from).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no app %q: %w", from, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE apps SET name = ? WHERE id = ?`, to, id); err != nil {
			return err
		}
		// A build's files are where its app's name says.
		oldPrefix, newPrefix := "builds/"+from+"/", "builds/"+to+"/"
		if _, err := tx.ExecContext(ctx, `UPDATE builds SET artifact_ref = ? || substr(artifact_ref, ?)
			WHERE app_id = ? AND artifact_kind = 'dir' AND artifact_ref LIKE ? || '%'`, newPrefix, len(oldPrefix)+1, id, oldPrefix); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE events SET app = ? WHERE app = ?`, to, from)
		return err
	})
}

// AppImages lists the images an app's builds were made into.
func (db *DB) AppImages(ctx context.Context, app string) ([]string, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT DISTINCT b.artifact_ref FROM builds b JOIN apps a ON a.id = b.app_id
		WHERE a.name = ? AND b.artifact_kind = 'image' AND b.collected_at IS NULL`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var image string
		if err := rows.Scan(&image); err != nil {
			return nil, err
		}
		out = append(out, image)
	}
	return out, rows.Err()
}
