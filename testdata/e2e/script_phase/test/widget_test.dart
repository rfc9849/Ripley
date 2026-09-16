import 'package:e2e_fixture/main.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  testWidgets('home screen exposes all smoke-test routes', (tester) async {
    await tester.pumpWidget(const PipelineCheckApp());

    expect(find.text('Ripley Pipeline Check'), findsOneWidget);
    expect(find.text('API feed'), findsOneWidget);
    expect(find.text('Native preferences'), findsOneWidget);
    expect(find.text('About this build'), findsOneWidget);
  });
}
