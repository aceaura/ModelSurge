import 'package:flutter/material.dart';
import 'package:shared_preferences/shared_preferences.dart';
import 'package:window_manager/window_manager.dart';

import 'api/client.dart';
import 'pages/dashboard_page.dart';
import 'pages/events_page.dart';
import 'pages/models_page.dart';
import 'pages/settings_page.dart';
import 'pages/upstreams_page.dart';
import 'theme/tokens.dart';

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();
  await windowManager.ensureInitialized();
  final prefs = await SharedPreferences.getInstance();
  const opts = WindowOptions(
    size: Size(1200, 800),
    minimumSize: Size(800, 600),
    titleBarStyle: TitleBarStyle.hidden,
    backgroundColor: Tokens.shell,
  );
  await windowManager.waitUntilReadyToShow(opts, () async {
    await windowManager.show();
    await windowManager.focus();
  });
  runApp(SurgeApp(prefs: prefs));
}

class SurgeApp extends StatefulWidget {
  final SharedPreferences prefs;
  const SurgeApp({super.key, required this.prefs});

  @override
  State<SurgeApp> createState() => _SurgeAppState();
}

class _SurgeAppState extends State<SurgeApp> {
  late final addrController = TextEditingController(
      text: widget.prefs.getString('admin_addr') ?? 'http://127.0.0.1:18099');
  late final tokenController =
      TextEditingController(text: widget.prefs.getString('admin_token') ?? '');
  ApiClient? api;

  @override
  void initState() {
    super.initState();
    _apply();
  }

  void _apply() {
    widget.prefs.setString('admin_addr', addrController.text.trim());
    widget.prefs.setString('admin_token', tokenController.text.trim());
    setState(() {
      api = ApiClient(addrController.text.trim(), tokenController.text.trim());
    });
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'ModelSurge',
      debugShowCheckedModeBanner: false,
      theme: Tokens.theme(),
      home: api == null
          ? const Scaffold(body: Center(child: CircularProgressIndicator()))
          : Shell(
              api: api!,
              addrController: addrController,
              tokenController: tokenController,
              onApply: _apply,
            ),
    );
  }
}

class Shell extends StatefulWidget {
  final ApiClient api;
  final TextEditingController addrController;
  final TextEditingController tokenController;
  final VoidCallback onApply;

  const Shell({
    super.key,
    required this.api,
    required this.addrController,
    required this.tokenController,
    required this.onApply,
  });

  @override
  State<Shell> createState() => _ShellState();
}

class _ShellState extends State<Shell> {
  int view = 0;

  static const titles = ['Dashboard', 'Upstreams', 'Models', 'Events', 'Settings'];
  static const icons = [
    Icons.dashboard_outlined,
    Icons.dns_outlined,
    Icons.view_list_outlined,
    Icons.terminal,
    Icons.settings_outlined,
  ];

  @override
  Widget build(BuildContext context) {
    final pages = [
      DashboardPage(api: widget.api),
      UpstreamsPage(api: widget.api),
      ModelsPage(api: widget.api),
      EventsPage(api: widget.api),
      SettingsPage(
        addrController: widget.addrController,
        tokenController: widget.tokenController,
        onApply: widget.onApply,
      ),
    ];

    return Scaffold(
      body: Column(
        children: [
          // custom title bar (drag region)
          GestureDetector(
            onPanStart: (_) => windowManager.startDragging(),
            child: Container(height: 8, color: Tokens.shell),
          ),
          Expanded(
            child: Row(
              children: [
                // dark sidebar
                Container(
                  width: 200,
                  decoration: const BoxDecoration(
                    color: Tokens.shell,
                    borderRadius: BorderRadius.only(
                      topLeft: Radius.circular(Tokens.cardRadius),
                      bottomLeft: Radius.circular(Tokens.cardRadius),
                    ),
                  ),
                  padding: const EdgeInsets.all(8),
                  child: Column(
                    children: [
                      for (var i = 0; i < titles.length; i++)
                        Padding(
                          padding: const EdgeInsets.symmetric(vertical: 2),
                          child: InkWell(
                            borderRadius: BorderRadius.circular(Tokens.navRadius),
                            hoverColor: Tokens.navHover,
                            onTap: () => setState(() => view = i),
                            child: AnimatedContainer(
                              duration: const Duration(milliseconds: 150),
                              height: 48,
                              padding: const EdgeInsets.symmetric(horizontal: 16),
                              decoration: BoxDecoration(
                                color: view == i ? Tokens.navActive : Colors.transparent,
                                borderRadius: BorderRadius.circular(Tokens.navRadius),
                              ),
                              child: Row(
                                children: [
                                  if (view == i)
                                    Container(
                                      width: 4,
                                      height: 24,
                                      margin: const EdgeInsets.only(right: 10),
                                      decoration: BoxDecoration(
                                        color: Tokens.accent,
                                        borderRadius: BorderRadius.circular(2),
                                      ),
                                    ),
                                  Icon(icons[i],
                                      size: 18,
                                      color: view == i ? Tokens.accent : Tokens.textMuted),
                                  const SizedBox(width: 12),
                                  Text(titles[i],
                                      style: TextStyle(
                                        color: view == i ? Colors.white : Tokens.textMuted,
                                        fontWeight:
                                            view == i ? FontWeight.bold : FontWeight.normal,
                                      )),
                                ],
                              ),
                            ),
                          ),
                        ),
                    ],
                  ),
                ),
                // light content area
                Expanded(
                  child: Container(
                    margin: const EdgeInsets.all(8),
                    decoration: BoxDecoration(
                      color: Tokens.contentBg,
                      borderRadius: BorderRadius.circular(Tokens.cardRadius),
                      boxShadow: const [
                        BoxShadow(color: Colors.black26, blurRadius: 24),
                      ],
                    ),
                    clipBehavior: Clip.antiAlias,
                    child: pages[view],
                  ),
                ),
              ],
            ),
          ),
        ],
      ),
    );
  }
}
