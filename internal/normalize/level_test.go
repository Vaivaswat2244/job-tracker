package normalize

import "testing"

func TestLevel(t *testing.T) {
	for _, tc := range []struct {
		name, title, employment, jd, want string
	}{
		// The ATS said so; nothing else needs consulting.
		{"ats says intern", "Software Engineer", "Intern", "", LevelIntern},
		{"ats fulltime is not a level", "Software Engineer", "FullTime", "", LevelUnknown},

		{"intern in title", "Software Engineer Intern (Fall 2026)", "", "", LevelIntern},
		{"trainee", "Engineering Trainee", "", "", LevelIntern},

		// The exact mistake a keyword filter makes: these are not internships.
		{"internal is not intern", "Software Engineer, Internal Tools", "", "", LevelUnknown},
		{"internal audit is not intern", "Full Stack Engineer - Internal Audit", "", "", LevelUnknown},
		{"international is not intern", "International Tax Reporting Lead", "", "", LevelSenior},

		{"new grad", "Software Engineer, New Grad", "", "", LevelJunior},
		{"entry level", "Entry-Level Backend Developer", "", "", LevelJunior},
		{"associate alone is junior", "Associate Software Engineer", "", "", LevelJunior},
		{"associate director is not", "Associate Director, Platform", "", "", LevelSenior},

		{"numeral I", "Software Engineer I", "", "", LevelJunior},
		{"numeral II", "Software Engineer II", "", "", LevelJunior},
		{"numeral III is mid", "Software Development Engineer III", "", "", LevelMid},
		{"numeral 2", "Security Engineer 2", "", "", LevelJunior},

		// A senior title beats a modest experience floor.
		{"senior with low years", "Senior Software Engineer, Workers Runtime", "",
			"You have 2+ years of experience", LevelSenior},
		{"staff", "Staff Engineer", "", "", LevelSenior},
		{"manager", "Engineering Manager", "", "", LevelSenior},

		// Experience supplies recall where the title says nothing.
		{"years only, junior", "Software Engineer", "", "1-3 years of experience required", LevelJunior},
		{"years only, mid", "Software Engineer", "", "4 years of experience", LevelMid},
		{"years only, senior", "Software Engineer", "", "8+ years of experience", LevelSenior},
		{"nothing at all", "Software Engineer", "", "", LevelUnknown},
	} {
		if got := Level(tc.title, tc.employment, tc.jd); got != tc.want {
			t.Errorf("%s: Level(%q) = %q, want %q", tc.name, tc.title, got, tc.want)
		}
	}
}

// A numeral is company-specific. "Engineer II" asking for four years is not a
// role a final-year student gets, whatever the numeral suggests.
func TestNumericLevelIsReconciledWithStatedYears(t *testing.T) {
	if got := Level("SIEM & SecOps Engineer II", "", "4+ years of experience in security"); got != LevelMid {
		t.Errorf("Level = %q, want mid — the numeral said junior but the posting asks for 4 years", got)
	}
	if got := Level("Engineer II", "", "8 years of experience"); got != LevelSenior {
		t.Errorf("Level = %q, want senior", got)
	}
	// ...and a genuine II with a low floor stays junior.
	if got := Level("Software Engineer II", "", "2+ years of experience"); got != LevelJunior {
		t.Errorf("Level = %q, want junior", got)
	}
}

func TestMinYears(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int
		ok   bool
	}{
		{"3-5 years of experience", 3, true},
		{"5+ years of experience", 5, true},
		{"at least 2 years experience", 2, true},
		{"1 to 3 years of relevant experience", 1, true},
		{"no numbers here", 0, false},
		{"99 years of experience", 0, false}, // nonsense, not a floor
	} {
		got, ok := MinYears(tc.text)
		if ok != tc.ok || got != tc.want {
			t.Errorf("MinYears(%q) = %d,%v want %d,%v", tc.text, got, ok, tc.want, tc.ok)
		}
	}
}

// Unknown must count as reachable. Hiding a role the classifier could not read
// is the failure mode that loses an opportunity silently (INV-1).
func TestEarlyCareerIncludesUnknown(t *testing.T) {
	for _, l := range []string{LevelIntern, LevelJunior, LevelUnknown, ""} {
		if !EarlyCareer(l) {
			t.Errorf("EarlyCareer(%q) = false, want true", l)
		}
	}
	for _, l := range []string{LevelMid, LevelSenior} {
		if EarlyCareer(l) {
			t.Errorf("EarlyCareer(%q) = true, want false", l)
		}
	}
}

func TestFunction(t *testing.T) {
	for _, tc := range []struct{ dept, title, want string }{
		{"Engineering", "Software Engineer", FunctionEngineering},
		{"Platform - Control Plane", "Backend Engineer", FunctionEngineering},
		{"R&D: Platform", "Backend Engineer", FunctionEngineering},
		{"Data Science", "ML Engineer", FunctionEngineering},

		// Contains "engineering" but is not an engineering job.
		{"Solution Engineering", "Solutions Engineer", FunctionOther},
		{"Sales Engineering", "Sales Engineer", FunctionOther},
		{"Field Engineering - Other", "Field Engineer", FunctionOther},

		{"Sales", "Account Executive", FunctionOther},
		{"Legal", "Counsel", FunctionOther},
		{"MFC Hourly", "Warehouse Associate", FunctionOther},
		{"Key Holders", "Key Holder", FunctionOther},

		// A pre-sales title inside a genuine engineering department.
		{"Engineering Development", "Product Solution Engineer", FunctionOther},

		{"", "Software Engineer", FunctionUnknown},
		{"All Cost Centers", "Something", FunctionUnknown},
	} {
		if got := Function(tc.dept, tc.title); got != tc.want {
			t.Errorf("Function(%q, %q) = %q, want %q", tc.dept, tc.title, got, tc.want)
		}
	}
}

func TestDepartmentPerATS(t *testing.T) {
	for _, tc := range []struct{ source, raw, want string }{
		{"greenhouse", `{"departments":[{"name":"Accounting and Finance"}]}`, "Accounting and Finance"},
		{"greenhouse", `{"departments":[]}`, ""},
		{"lever", `{"categories":{"team":"Sales","department":"AngelList"}}`, "Sales"},
		{"lever", `{"categories":{"department":"Engineering"}}`, "Engineering"},
		{"ashby", `{"department":"Sales","team":"Sales"}`, "Sales"},
		{"ashby", `{"team":"Platform"}`, "Platform"},
		{"greenhouse", `not json`, ""},
		{"greenhouse", ``, ""},
	} {
		if got := Department(tc.source, tc.raw); got != tc.want {
			t.Errorf("Department(%s) = %q, want %q", tc.source, got, tc.want)
		}
	}
}

func TestEmploymentType(t *testing.T) {
	for _, tc := range []struct{ source, raw, want string }{
		{"lever", `{"categories":{"commitment":"Intern"}}`, "Intern"},
		{"ashby", `{"employmentType":"FullTime"}`, "FullTime"},
		{"greenhouse", `{"metadata":[{"name":"Employment Type","value":"Intern"}]}`, "Intern"},
		{"greenhouse", `{"metadata":[{"name":"Cost Center","value":"6991"}]}`, ""},
	} {
		if got := EmploymentType(tc.source, tc.raw); got != tc.want {
			t.Errorf("EmploymentType(%s) = %q, want %q", tc.source, got, tc.want)
		}
	}
}
