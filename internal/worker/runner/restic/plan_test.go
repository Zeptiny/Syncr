package restic

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// remoteGlobals are the arguments every command on remote repository bk has.
func remoteGlobals(cached bool) []string {
	g := []string{
		"--repo=rclone:bk:bucket/repo one",
		"--option=rclone.program=" + fakeRcloneBin,
		"--option=rclone.args=serve restic --stdio --config= --b2-hard-delete --drive-use-trash=false",
		"--json",
	}
	if !cached {
		g = append(g, "--no-cache")
	}
	return g
}

func remoteSpec(op func(*syncrv1.JobSpec)) *syncrv1.JobSpec {
	return specWith([]*syncrv1.RemoteDefinition{s3Remote()}, op)
}

func TestArgs(t *testing.T) {
	t.Parallel()
	// build returns the spec and the expected arguments of the restic child.
	tests := []struct {
		name  string
		build func(te *testEnv) (*syncrv1.JobSpec, []string)
	}{
		{"init", func(*testEnv) (*syncrv1.JobSpec, []string) {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticInit{ResticInit: &syncrv1.ResticInit{Repository: remoteRepo()}}
			}), append(remoteGlobals(false), "init")
		}},
		{"backup minimal", func(te *testEnv) (*syncrv1.JobSpec, []string) {
			src := te.local("docs")
			return backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{src}}),
				append(remoteGlobals(true), "backup", "--", src)
		}},
		{"backup full", func(te *testEnv) (*syncrv1.JobSpec, []string) {
			src1, src2 := te.local("docs"), te.local("photos", "2024")
			return backupSpec(remoteRepo(), &syncrv1.ResticBackup{
					Paths:                  []string{src1, src2},
					Excludes:               []string{"*.tmp", "--password-file=/etc/shadow", "node_modules"},
					Tags:                   []string{"nightly", "docs"},
					Host:                   "laptop",
					ExcludeCaches:          true,
					OneFileSystem:          true,
					ExcludeLargerThanBytes: proto.Int64(1 << 30),
					DryRun:                 true,
				}), append(remoteGlobals(true), "backup",
					"--tag=nightly", "--tag=docs", "--host=laptop",
					"--exclude=*.tmp", "--exclude=--password-file=/etc/shadow", "--exclude=node_modules",
					"--exclude-caches", "--one-file-system", "--exclude-larger-than=1073741824", "--dry-run",
					"--", src1, src2)
		}},
		{"forget", func(*testEnv) (*syncrv1.JobSpec, []string) {
			return remoteSpec(func(s *syncrv1.JobSpec) {
					s.Operation = &syncrv1.JobSpec_ResticForget{ResticForget: &syncrv1.ResticForget{
						Repository: remoteRepo(),
						Policy: &syncrv1.ResticRetentionPolicy{
							KeepLast: 3, KeepHourly: 24, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12, KeepYearly: 2,
							KeepTags: []string{"keep"},
						},
						Prune: true, Tags: []string{"nightly"}, Host: "laptop", DryRun: true,
					}}
				}), append(remoteGlobals(true), "forget",
					"--keep-last=3", "--keep-hourly=24", "--keep-daily=7", "--keep-weekly=4", "--keep-monthly=12", "--keep-yearly=2",
					"--keep-tag=keep", "--tag=nightly", "--host=laptop", "--prune", "--dry-run")
		}},
		{"prune", func(*testEnv) (*syncrv1.JobSpec, []string) {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{Repository: remoteRepo()}}
			}), append(remoteGlobals(true), "prune")
		}},
		{"prune dry run", func(*testEnv) (*syncrv1.JobSpec, []string) {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{Repository: remoteRepo(), DryRun: true}}
			}), append(remoteGlobals(true), "prune", "--dry-run")
		}},
		{"check structure", func(*testEnv) (*syncrv1.JobSpec, []string) {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticCheck{ResticCheck: &syncrv1.ResticCheck{Repository: remoteRepo()}}
			}), append(remoteGlobals(true), "check")
		}},
		{"check data subset", func(*testEnv) (*syncrv1.JobSpec, []string) {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticCheck{ResticCheck: &syncrv1.ResticCheck{Repository: remoteRepo(), ReadDataPercent: 25}}
			}), append(remoteGlobals(true), "check", "--read-data-subset=25%")
		}},
		{"restore latest", func(te *testEnv) (*syncrv1.JobSpec, []string) {
			target := te.local("restore")
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
					Repository: remoteRepo(), SnapshotId: "latest", Target: target,
				}}
			}), append(remoteGlobals(true), "restore", "--target="+target, "--", "latest")
		}},
		{"restore full", func(te *testEnv) (*syncrv1.JobSpec, []string) {
			target := te.local("restore")
			return remoteSpec(func(s *syncrv1.JobSpec) {
					s.Operation = &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
						Repository: remoteRepo(), SnapshotId: "948693b6", Target: target,
						Includes: []string{"/srv/docs"}, Excludes: []string{"*.log"}, Verify: true,
					}}
				}), append(remoteGlobals(true), "restore", "--target="+target,
					"--include=/srv/docs", "--exclude=*.log", "--verify", "--", "948693b6")
		}},
		{"snapshots", func(*testEnv) (*syncrv1.JobSpec, []string) {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticSnapshots{ResticSnapshots: &syncrv1.ResticSnapshots{Repository: remoteRepo()}}
			}), append(remoteGlobals(true), "snapshots")
		}},
		{"snapshots filtered", func(*testEnv) (*syncrv1.JobSpec, []string) {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticSnapshots{ResticSnapshots: &syncrv1.ResticSnapshots{
					Repository: remoteRepo(), Tags: []string{"a", "b"}, Host: "laptop",
				}}
			}), append(remoteGlobals(true), "snapshots", "--tag=a", "--tag=b", "--host=laptop")
		}},
		{"local repository", func(te *testEnv) (*syncrv1.JobSpec, []string) {
			repo := te.local("repo")
			return specWith(nil, func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{Repository: localRepo(repo)}}
			}), []string{"--repo=" + repo, "--json", "prune"}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			te.sup.exited(0).stdout = []string{`{"message_type":"summary","snapshot_id":"abc"}`, `[]`}
			spec, want := tc.build(te)
			// Parsing the canned output may fail for some kinds; only the
			// child's arguments are under test here.
			_, _, _ = te.run(spec)
			if te.sup.called != 1 {
				t.Fatalf("supervisor called %d times, want 1", te.sup.called)
			}
			if te.sup.spec.Path != fakeResticBin {
				t.Errorf("Path = %q, want %q", te.sup.spec.Path, fakeResticBin)
			}
			// A local repository is the only case without the cache flag
			// decision encoded in remoteGlobals; the cache is on for it.
			if !slices.Equal(te.sup.spec.Args, want) {
				t.Errorf("args =\n  %q\nwant\n  %q", te.sup.spec.Args, want)
			}
		})
	}
}

func TestVSSOnlyOnWindows(t *testing.T) {
	t.Parallel()
	spec := func(te *testEnv) *syncrv1.JobSpec {
		return backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("c")}, UseFsSnapshot: true})
	}

	t.Run("linux rejects", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		setGOOS(t, te, "linux")
		_, _, err := te.run(spec(te))
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC)
		if te.sup.called != 0 {
			t.Error("restic must not start for a rejected spec")
		}
	})
	t.Run("windows passes the flag", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		setGOOS(t, te, "windows")
		te.sup.exited(0).stdout = []string{`{"message_type":"summary","snapshot_id":"abc"}`}
		if _, _, err := te.run(spec(te)); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(te.sup.spec.Args, "--use-fs-snapshot") {
			t.Errorf("args = %q, want --use-fs-snapshot", te.sup.spec.Args)
		}
	})
}

// setGOOS makes every runner of te behave as if running on goos.
func setGOOS(t *testing.T, te *testEnv, goos string) {
	t.Helper()
	for _, r := range te.runners {
		if kr, ok := r.(*kindRunner); ok {
			kr.e.goos = goos // one engine is shared by every runner
		}
	}
}

func TestInvalidSpecs(t *testing.T) {
	t.Parallel()
	invalid := syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC
	tests := []struct {
		name  string
		build func(te *testEnv) *syncrv1.JobSpec
		code  syncrv1.JobErrorCode
	}{
		{"backup without paths", func(*testEnv) *syncrv1.JobSpec {
			return backupSpec(remoteRepo(), &syncrv1.ResticBackup{})
		}, invalid},
		{"backup tag with comma", func(te *testEnv) *syncrv1.JobSpec {
			return backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("a")}, Tags: []string{"a,b"}})
		}, invalid},
		{"backup empty tag", func(te *testEnv) *syncrv1.JobSpec {
			return backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("a")}, Tags: []string{""}})
		}, invalid},
		{"backup exclude with newline", func(te *testEnv) *syncrv1.JobSpec {
			return backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("a")}, Excludes: []string{"a\nb"}})
		}, invalid},
		{"backup size limit not positive", func(te *testEnv) *syncrv1.JobSpec {
			return backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("a")}, ExcludeLargerThanBytes: proto.Int64(0)})
		}, invalid},
		{"backup host with control character", func(te *testEnv) *syncrv1.JobSpec {
			return backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("a")}, Host: "a\x00b"})
		}, invalid},
		{"backup relative path", func(*testEnv) *syncrv1.JobSpec {
			return backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{"relative/dir"}})
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{"backup path outside local_roots", func(te *testEnv) *syncrv1.JobSpec {
			return backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("a"), te.root + "-other"}})
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{"backup path traversal", func(te *testEnv) *syncrv1.JobSpec {
			return backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("a", "..", "..", "etc")}})
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{"forget without a rule", func(*testEnv) *syncrv1.JobSpec {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticForget{ResticForget: &syncrv1.ResticForget{
					Repository: remoteRepo(), Policy: &syncrv1.ResticRetentionPolicy{},
				}}
			})
		}, invalid},
		{"forget without a policy", func(*testEnv) *syncrv1.JobSpec {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticForget{ResticForget: &syncrv1.ResticForget{Repository: remoteRepo(), Prune: true}}
			})
		}, invalid},
		{"forget negative rule", func(*testEnv) *syncrv1.JobSpec {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticForget{ResticForget: &syncrv1.ResticForget{
					Repository: remoteRepo(), Policy: &syncrv1.ResticRetentionPolicy{KeepLast: 1, KeepDaily: -1},
				}}
			})
		}, invalid},
		{"check percent above 100", func(*testEnv) *syncrv1.JobSpec {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticCheck{ResticCheck: &syncrv1.ResticCheck{Repository: remoteRepo(), ReadDataPercent: 101}}
			})
		}, invalid},
		{"restore snapshot id looks like a flag", func(te *testEnv) *syncrv1.JobSpec {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
					Repository: remoteRepo(), SnapshotId: "--target=/etc", Target: te.local("r"),
				}}
			})
		}, invalid},
		{"restore snapshot id with subfolder", func(te *testEnv) *syncrv1.JobSpec {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
					Repository: remoteRepo(), SnapshotId: "latest:/etc", Target: te.local("r"),
				}}
			})
		}, invalid},
		{"restore without snapshot id", func(te *testEnv) *syncrv1.JobSpec {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
					Repository: remoteRepo(), Target: te.local("r"),
				}}
			})
		}, invalid},
		{"restore target outside local_roots", func(te *testEnv) *syncrv1.JobSpec {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
					Repository: remoteRepo(), SnapshotId: "latest", Target: te.root + "-other",
				}}
			})
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{"local repository outside local_roots", func(te *testEnv) *syncrv1.JobSpec {
			return specWith(nil, func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticInit{ResticInit: &syncrv1.ResticInit{Repository: localRepo(te.root + "-other")}}
			})
		}, syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED},
		{"repository without password", func(*testEnv) *syncrv1.JobSpec {
			repo := remoteRepo()
			repo.Password = ""
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{Repository: repo}}
			})
		}, invalid},
		{"repository without location", func(*testEnv) *syncrv1.JobSpec {
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{Repository: &syncrv1.ResticRepository{Password: "x"}}}
			})
		}, invalid},
		{"repository remote not defined", func(*testEnv) *syncrv1.JobSpec {
			return specWith(nil, func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{Repository: remoteRepo()}}
			})
		}, invalid},
		{"repository alias with colon", func(*testEnv) *syncrv1.JobSpec {
			repo := remoteRepo()
			repo.Location.Remote = "bk:evil"
			return remoteSpec(func(s *syncrv1.JobSpec) {
				s.Operation = &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{Repository: repo}}
			})
		}, invalid},
		{"no operation", func(*testEnv) *syncrv1.JobSpec { return &syncrv1.JobSpec{} }, invalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			spec := tc.build(te)
			kind := kindOf(spec)
			if kind == "" {
				kind = kindPrune
			}
			_, err := te.runners[kind].Run(t.Context(), runnerJob(spec), func(_ runnerProgress) {})
			mustFail(t, err, tc.code)
			if te.sup.called != 0 {
				t.Errorf("restic started for an invalid spec; args %q", te.sup.spec.Args)
			}
		})
	}
}

func TestWrongOperationForRunner(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	spec := remoteSpec(func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{Repository: remoteRepo()}}
	})
	_, err := te.runners[kindBackup].Run(t.Context(), runnerJob(spec), func(runnerProgress) {})
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC)
}
