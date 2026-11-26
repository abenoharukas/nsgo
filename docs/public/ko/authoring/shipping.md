---
title: 단어, 테스트 및 배포
description: 컴포넌트의 문구를 번역하고, 사용자처럼 테스트하며, Go 및 TSX 앱에 제공합니다.
group: 컴포넌트 작성
order: 74
---

## 단어

스크린 리더용 버튼 이름처럼 컴포넌트가 직접 말하는 문구는 문자열 리터럴 대신 패키지의 카탈로그를 통해 제공하세요. 라이브러리에 원본 언어 카탈로그를 담은 `locales/` 패키지와 코드 생성을 위한 파일을 추가합니다:

```yaml
# locales/en.yaml
clear:
  message: Clear
  description: What screen readers say of the button that empties a chip input.
```

```go
// Package locales says the chips' words in the language the app shows.
package locales

//go:generate go tool nsgo i18n
```

`go generate ./locales`를 실행하면 메시지마다 함수가 생성되며, 컴포넌트는 스코프와 함께 이를 호출합니다. 예를 들어 `locales.Clear(cx)`는 앱이 표시 중인 언어로 해당 문구를 반환합니다. 앱이 Go 도구만으로 빌드할 수 있도록 생성된 `catalog_gen.go`도 커밋하세요. 번역할 언어마다 파일(`zh-Hans.yaml` 등)을 추가하며, 앱은 [NSGo 자체 문구](../localization.md#nsgo-기본-문구)와 마찬가지로 패키지 임포트 경로를 키로 삼아 자체 카탈로그에서 다른 언어를 제공할 수 있습니다.

## 테스트

`nsgotest`는 사용자가 사용하는 방식 그대로 컴포넌트를 헤드리스(headless)로 테스트합니다. 작은 루트에 컴포넌트를 마운트하고 역할과 텍스트로 상호작용하세요:

```go
func TestMain(m *testing.M) { nsgotest.Main(m) }

func TestToggle(t *testing.T) {
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		on := core.NewSignal(cx, false)
		return chips.Toggle{On: on.Get, OnChange: on.Set, Label: "Bold"}
	})
	app.Press("role:switch Bold")
	if !app.Find("role:switch Bold").Props.Checked {
		t.Fatal("the toggle did not turn on")
	}
}
```

라이브러리 루트에서 `go tool nsgo test`를 실행하면 모든 패키지를 테스트하며, `-target macos` 같은 타겟을 지정하면 네이티브 창에서 동일한 테스트를 실행합니다. 하드코딩된 색상이나 문구를 잡아내려면 라이트/다크 모드 양쪽과 의사 로케일(`-lang en-XA`)을 모두 테스트하세요. 자세한 내용은 [테스트](../testing.md)를 참고하세요.

## 배포

컴포넌트 라이브러리는 Go 모듈이므로 앱이 `go get`으로 받아 패키지를 임포트합니다. 스타일 없는 버전과 스타일이 적용된 버전이 모두 있을 때는 서로 다른 패키지로 분리해 두어, 독자적인 외형을 쓰는 앱이 사용하지 않을 스타일을 임포트하지 않도록 하세요.

사이트의 [컴포넌트](https://nsgo.dev/components/) 페이지는 다른 개발자들이 사용할 수 있는 라이브러리를 소개합니다. 여러분의 라이브러리를 올리려면 `docs/public/components/`에 문서와 스크린샷을 추가하세요.

TSX 앱은 `init` 함수에서 등록된 Go 컴포넌트를 사용할 수 있습니다:

```go
func init() {
	tsx.Register("example.com/chips", map[string]core.Node{"Toggle": Toggle{}})
}
```

이후 `import { Toggle } from "example.com/chips"`로 불러오면 구조체 필드를 props로 전달할 수 있습니다([TSX 앱](../tsx.md#tsx에서-go-호출하기)).

## nsgo-module.yaml

라이브러리는 모듈 루트의 `go.mod` 옆에 두는 선택 파일 `nsgo-module.yaml`로, 자신을 추가하는 앱이 알아야 할 것을 알립니다. [`nsgo add`](../building.md#라이브러리-추가)는 무엇이든 쓰기 전에 계획에서 이 내용을 보여 줍니다.

```yaml
version: 1
import: [player]            # 앱이 임포트하는 패키지, 모듈 루트 기준 상대 경로이며 .은 루트
targets: [macos, windows_x64, linux_x64, ios, ios_simulator]
notes: |
  FFmpeg 라이브러리는 LGPL 2.1입니다. 다운로드 옆에 소스를 제공하고, FFmpeg를 표기하고, 라이브러리 교체를 허용하세요.
components:                 # 모듈이 복사용으로 제공하는 패키지
  chat: ui/chat
```

- `import`는 앱이 임포트하는 패키지를 나열합니다. 각각은 이 모듈의 패키지여야 하며, 그 안에 중첩된 모듈의 패키지나 `main` 패키지는 안 됩니다. 계획은 각각에 대해 import 줄을 출력하고, 이 필드나 파일이 없으면 루트 패키지의 줄을 출력합니다.
- `targets`는 `build.targets`처럼 타겟 이름과 그룹을 받습니다. 계획은 앱의 타겟 중 이 목록에 없는 것을 알려 주고, `nsgo check -target`은 앱이 링크하는 모듈의 목록에 검사 중인 타겟이 없으면 경고합니다. 이 필드가 없으면 둘 다 그 모듈에 대해 아무 말도 하지 않습니다.
- `notes`는 줄바꿈 외의 제어 문자나 서식 문자가 없는 최대 4 KiB의 일반 텍스트입니다. 계획의 맨 끝, `From <module>, the author's notes:` 아래에 각 줄마다 `|` 표시를 달고 나옵니다. 라이선스 의무처럼 앱이 져야 할 일을 적는 곳입니다.
- `components`는 1~64자의 소문자 ASCII 영문자, 숫자, 하이픈으로 되고 하이픈으로 시작하거나 끝나지 않는 이름을 모듈 루트 아래의 패키지 디렉터리에 대응시킵니다. 각 디렉터리는 이 모듈의 `main`이 아닌 패키지이고, 마지막 요소가 그 패키지 이름과 같습니다. 계획은 각 컴포넌트를 `nsgo add <module>#<name>`과 함께 나열합니다. nsgo는 아직 컴포넌트를 복사하지 못하며 당분간 이 명령을 거부합니다.

이 파일은 엄격합니다. 최대 1 MiB의 YAML 문서 하나, 버전 1이어야 하며, 앵커와 별칭, nsgo가 모르는 필드는 허용되지 않습니다. `nsgo add`는 받아들일 수 없는 파일을 가진 모듈을 `-y` 여부와 관계없이 거부합니다. 새 필드는 버전 1 안에서 추가되므로, 새 필드를 쓰는 모듈은 그것을 읽는 NSGo를 요구하고, 더 오래된 nsgo는 그 NSGo를 가져오는 `go get github.com/nsgo-dev/nsgo@<버전>`을 알려 줍니다. `nsgo build`는 이 파일을 읽지 않습니다.
