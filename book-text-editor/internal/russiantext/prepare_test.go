package russiantext

import "testing"

func TestPrepareSelectiveStressOnlyUsesAcceptedContexts(t *testing.T) {
	t.Parallel()
	source := "Старинный замок стоял на вершине холма, а дверной замок был закрыт. " +
		"На столе лежала белая мука, а в душе росла невыносимая мука. " +
		"Я плачу за квартиру и плачу потому, что уезжаю. Неизвестный замок остался без подсказки."
	got, report := Prepare(source, Options{SelectiveStress: true})
	want := "Старинный за́мок стоял на вершине холма́, а дверной замо́к был закрыт. " +
		"На столе лежала белая мука́, а в душе росла невыносимая му́ка. " +
		"Я плачу́ за квартиру и пла́чу потому, что уезжаю. Неизвестный замок остался без подсказки."
	if got != want {
		t.Fatalf("Prepare() = %q, want %q", got, want)
	}
	if report.SelectiveStressApplied != 7 || report.MorphologyReplacements != 0 {
		t.Fatalf("report = %+v", report)
	}
}

func TestPrepareMorphologyRegressionCorpus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{
			name:   "gender and ordinal",
			source: "В комнате стоял 21 стул и лежала 21 книга. Он занял 21-й ряд, вернулся к 21-му пункту и не дошёл до 21-го километра.",
			want:   "В комнате стоял двадцать один стул и лежала двадцать одна книга. Он занял двадцать первый ряд, вернулся к двадцать первому пункту и не дошёл до двадцать первого километра.",
		},
		{
			name:   "units",
			source: "Посылка весила 5 кг, влажность достигла 3 %, а машина двигалась со скоростью 10 км/ч. До станции оставалось 12 км.",
			want:   "Посылка весила пять килограммов, влажность достигла трёх процентов, а машина двигалась со скоростью десять километров в час. До станции оставалось двенадцать километров.",
		},
		{
			name:   "decimals and grouping",
			source: "В отчёте указали 12 500 участников, 1,5 литра воды и коэффициент 3.14. Из них 2026 человек ответили на 21 вопрос.",
			want:   "В отчёте указали двенадцать тысяч пятьсот участников, одна целая пять десятых литра воды и коэффициент три целых четырнадцать сотых. Из них две тысячи двадцать шесть человек ответили на двадцать один вопрос.",
		},
		{
			name:   "headings preserve unsupported roman century",
			source: "Книга XV. Глава 7. В XVIII веке всё изменилось.",
			want:   "Книга пятнадцатая. Глава седьмая. В XVIII веке всё изменилось.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, report := Prepare(test.source, Options{NormalizeMorphology: true})
			if got != test.want {
				t.Fatalf("Prepare() = %q\nwant      = %q", got, test.want)
			}
			if report.MorphologyReplacements == 0 {
				t.Fatalf("report = %+v", report)
			}
		})
	}
}

func TestPrepareDisabledIsIdentity(t *testing.T) {
	t.Parallel()
	const source = "Глава 7. Неизвестный замок и 21 книга."
	got, report := Prepare(source, Options{})
	if got != source || report.SelectiveStressApplied != 0 || report.MorphologyReplacements != 0 {
		t.Fatalf("Prepare() = (%q, %+v)", got, report)
	}
}

func TestPrepareMorphologyLeavesUnsupportedContextsUnchanged(t *testing.T) {
	t.Parallel()
	const source = "С 21 книгой он пришёл к версии 3.14.159 и занял место 404. Пётр I молчал."
	got, report := Prepare(source, Options{NormalizeMorphology: true})
	if got != source || report.MorphologyReplacements != 0 {
		t.Fatalf("Prepare() = (%q, %+v), want fail-closed identity", got, report)
	}
}

func TestPrepareMorphologyLeavesListenerRejectedDatesAndCenturiesUnchanged(t *testing.T) {
	t.Parallel()
	const source = "18.08.2026, 18 августа 2026 года, в 1941 году, до 2026 года и XVIII век."
	got, report := Prepare(source, Options{NormalizeMorphology: true})
	if got != source || report.MorphologyReplacements != 0 {
		t.Fatalf("Prepare() = (%q, %+v), want listener-approved identity", got, report)
	}
}

func TestPrepareSelectiveStressLeavesListenerRejectedHomographsUnchanged(t *testing.T) {
	t.Parallel()
	const source = "Стрелки часов замерли, а стрелки отряда ждали. Рыжие белки и белки из образца."
	got, report := Prepare(source, Options{SelectiveStress: true})
	if got != source || report.SelectiveStressApplied != 0 {
		t.Fatalf("Prepare() = (%q, %+v), want manual-review identity", got, report)
	}
}

func TestPrepareIsDeterministic(t *testing.T) {
	t.Parallel()
	const source = "Глава 7. На столе лежала белая мука и 21 книга."
	options := Options{SelectiveStress: true, NormalizeMorphology: true}
	first, firstReport := Prepare(source, options)
	second, secondReport := Prepare(source, options)
	if first != second || firstReport != secondReport {
		t.Fatalf("non-deterministic results: (%q, %+v) vs (%q, %+v)", first, firstReport, second, secondReport)
	}
}
