import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:http/http.dart' as http;
import 'package:shared_preferences/shared_preferences.dart';
import 'package:vendored_binary_plugin/vendored_binary_plugin.dart';

void main() {
  runApp(const PipelineCheckApp());
}

class PipelineCheckApp extends StatelessWidget {
  const PipelineCheckApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      debugShowCheckedModeBanner: false,
      title: 'Ripley Pipeline Check $vendoredBinaryFixtureMarker',
      theme: ThemeData(
        colorScheme: ColorScheme.fromSeed(seedColor: Colors.indigo),
        useMaterial3: true,
      ),
      home: const HomeScreen(),
    );
  }
}

class HomeScreen extends StatelessWidget {
  const HomeScreen({super.key});

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Ripley Pipeline Check')),
      body: ListView(
        padding: const EdgeInsets.all(20),
        children: [
          Text(
            'End-to-end smoke test',
            style: Theme.of(context).textTheme.headlineMedium,
          ),
          const SizedBox(height: 8),
          Text(
            'This app exercises Flutter UI, navigation, HTTPS fetching, JSON parsing, and a native iOS plugin.',
            style: Theme.of(context).textTheme.bodyLarge,
          ),
          const SizedBox(height: 24),
          _NavCard(
            icon: Icons.cloud_download_outlined,
            title: 'API feed',
            subtitle: 'Fetch posts from JSONPlaceholder over HTTPS',
            onTap: () => Navigator.of(context)
                .push(MaterialPageRoute(builder: (_) => const ApiFeedScreen())),
          ),
          const SizedBox(height: 12),
          _NavCard(
            icon: Icons.storage_outlined,
            title: 'Native preferences',
            subtitle: 'Read and write shared_preferences on iOS',
            onTap: () => Navigator.of(context).push(
              MaterialPageRoute(builder: (_) => const PreferencesScreen()),
            ),
          ),
          const SizedBox(height: 12),
          _NavCard(
            icon: Icons.info_outline,
            title: 'About this build',
            subtitle: 'What this smoke test validates',
            onTap: () => Navigator.of(context)
                .push(MaterialPageRoute(builder: (_) => const AboutScreen())),
          ),
        ],
      ),
    );
  }
}

class _NavCard extends StatelessWidget {
  const _NavCard({
    required this.icon,
    required this.title,
    required this.subtitle,
    required this.onTap,
  });

  final IconData icon;
  final String title;
  final String subtitle;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    return Card(
      clipBehavior: Clip.antiAlias,
      child: ListTile(
        contentPadding: const EdgeInsets.symmetric(
          horizontal: 18,
          vertical: 10,
        ),
        leading: Icon(icon, size: 30),
        title: Text(title),
        subtitle: Text(subtitle),
        trailing: const Icon(Icons.chevron_right),
        onTap: onTap,
      ),
    );
  }
}

class ApiFeedScreen extends StatefulWidget {
  const ApiFeedScreen({super.key});

  @override
  State<ApiFeedScreen> createState() => _ApiFeedScreenState();
}

class _ApiFeedScreenState extends State<ApiFeedScreen> {
  static final Uri _endpoint = Uri.parse(
    'https://jsonplaceholder.typicode.com/posts?_limit=12',
  );

  late Future<List<Post>> _posts;

  @override
  void initState() {
    super.initState();
    _posts = _fetchPosts();
  }

  Future<List<Post>> _fetchPosts() async {
    final response = await http
        .get(_endpoint)
        .timeout(const Duration(seconds: 12));
    if (response.statusCode != 200) {
      throw Exception('HTTP ${response.statusCode}');
    }

    final data = jsonDecode(response.body) as List<dynamic>;
    return data
        .map((item) => Post.fromJson(item as Map<String, dynamic>))
        .toList(growable: false);
  }

  void _retry() {
    setState(() {
      _posts = _fetchPosts();
    });
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(
        title: const Text('API feed'),
        actions: [
          IconButton(
            tooltip: 'Reload',
            onPressed: _retry,
            icon: const Icon(Icons.refresh),
          ),
        ],
      ),
      body: FutureBuilder<List<Post>>(
        future: _posts,
        builder: (context, snapshot) {
          if (snapshot.connectionState == ConnectionState.waiting) {
            return const Center(child: CircularProgressIndicator());
          }
          if (snapshot.hasError) {
            return Center(
              child: Padding(
                padding: const EdgeInsets.all(24),
                child: Column(
                  mainAxisSize: MainAxisSize.min,
                  children: [
                    const Icon(Icons.cloud_off, size: 48),
                    const SizedBox(height: 12),
                    Text(
                      'Fetch failed\n${snapshot.error}',
                      textAlign: TextAlign.center,
                    ),
                    const SizedBox(height: 16),
                    FilledButton.icon(
                      onPressed: _retry,
                      icon: const Icon(Icons.refresh),
                      label: const Text('Retry'),
                    ),
                  ],
                ),
              ),
            );
          }

          final posts = snapshot.data ?? const <Post>[];
          return ListView.separated(
            padding: const EdgeInsets.all(12),
            itemCount: posts.length,
            separatorBuilder: (_, _) => const SizedBox(height: 8),
            itemBuilder: (context, index) {
              final post = posts[index];
              return Card(
                child: ListTile(
                  leading: CircleAvatar(child: Text('${post.id}')),
                  title: Text(post.title),
                  subtitle: Text(
                    post.body,
                    maxLines: 2,
                    overflow: TextOverflow.ellipsis,
                  ),
                  trailing: const Icon(Icons.chevron_right),
                  onTap: () => Navigator.of(context).push(
                    MaterialPageRoute(
                      builder: (_) => PostDetailScreen(post: post),
                    ),
                  ),
                ),
              );
            },
          );
        },
      ),
    );
  }
}

class PostDetailScreen extends StatelessWidget {
  const PostDetailScreen({super.key, required this.post});

  final Post post;

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: Text('Post ${post.id}')),
      body: Padding(
        padding: const EdgeInsets.all(20),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(post.title, style: Theme.of(context).textTheme.headlineSmall),
            const SizedBox(height: 16),
            Text(post.body, style: Theme.of(context).textTheme.bodyLarge),
          ],
        ),
      ),
    );
  }
}

class PreferencesScreen extends StatefulWidget {
  const PreferencesScreen({super.key});

  @override
  State<PreferencesScreen> createState() => _PreferencesScreenState();
}

class _PreferencesScreenState extends State<PreferencesScreen> {
  static const _key = 'pipeline_switch';
  bool _enabled = false;
  bool _loading = true;

  @override
  void initState() {
    super.initState();
    _load();
  }

  Future<void> _load() async {
    final prefs = await SharedPreferences.getInstance();
    if (!mounted) return;
    setState(() {
      _enabled = prefs.getBool(_key) ?? false;
      _loading = false;
    });
  }

  Future<void> _setEnabled(bool value) async {
    final prefs = await SharedPreferences.getInstance();
    await prefs.setBool(_key, value);
    if (!mounted) return;
    setState(() => _enabled = value);
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Native preferences')),
      body: _loading
          ? const Center(child: CircularProgressIndicator())
          : ListView(
              padding: const EdgeInsets.all(20),
              children: [
                SwitchListTile(
                  value: _enabled,
                  onChanged: _setEnabled,
                  title: const Text('Persistent test switch'),
                  subtitle: const Text(
                    'Toggle this, leave the screen, and return. The value is stored through shared_preferences.',
                  ),
                ),
                const SizedBox(height: 16),
                Card(
                  child: Padding(
                    padding: const EdgeInsets.all(16),
                    child: Text(
                      _enabled ? 'Stored value: ON' : 'Stored value: OFF',
                    ),
                  ),
                ),
              ],
            ),
    );
  }
}

class AboutScreen extends StatelessWidget {
  const AboutScreen({super.key});

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('About this build')),
      body: ListView(
        padding: const EdgeInsets.all(20),
        children: const [
          ListTile(
            leading: Icon(Icons.flutter_dash),
            title: Text('Flutter UI + navigation'),
            subtitle: Text('Multiple Material screens and route transitions'),
          ),
          ListTile(
            leading: Icon(Icons.public),
            title: Text('HTTPS + JSON'),
            subtitle: Text('package:http fetching a public REST endpoint'),
          ),
          ListTile(
            leading: Icon(Icons.extension),
            title: Text('Native iOS plugin'),
            subtitle: Text(
              'shared_preferences_foundation compiled into the app',
            ),
          ),
          ListTile(
            leading: Icon(Icons.verified_user_outlined),
            title: Text('Real device signing'),
            subtitle: Text('Bundle id: com.example.ripley.fixture'),
          ),
        ],
      ),
    );
  }
}

class Post {
  const Post({required this.id, required this.title, required this.body});

  final int id;
  final String title;
  final String body;

  factory Post.fromJson(Map<String, dynamic> json) {
    return Post(
      id: json['id'] as int,
      title: json['title'] as String,
      body: json['body'] as String,
    );
  }
}
