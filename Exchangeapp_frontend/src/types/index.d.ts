export interface User {
  id: number;
  username: string;
  avatar: string;
  bio: string;
  article_count?: number;
  follower_count?: number;
  following_count?: number;
  created_at?: string;
}

export interface Article {
  ID: number;
  CreatedAt: string;
  UpdatedAt: string;
  Title: string;
  Content: string;
  Preview: string;
  AuthorID: number;
  Author: User;
  Status: string;
  ViewCount: number;
  Category: string;
  LikeCount: number;
  CommentCount: number;
}

export interface Comment {
  id: number;
  content: string;
  user: User;
  article_id: number;
  like_count: number;
  created_at: string;
  replies: Comment[];
}

export interface CommentListResponse {
  comments: Comment[];
  total: number;
}

export interface LikeResponse {
  liked: boolean;
  like_count: number;
}

export interface Notification {
  ID: number;
  CreatedAt: string;
  UserID: number;
  FromUserID: number;
  FromUser: User;
  Type: string;
  TargetID: number;
  Content: string;
  IsRead: boolean;
}

export interface PaginatedResponse<T> {
  items: T[];
  total: number;
  page: number;
  size: number;
}

export interface FeedItem extends Article {}
