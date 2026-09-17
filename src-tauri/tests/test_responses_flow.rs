use model_bridge_lib::converter;
use model_bridge_lib::converter::stream::StreamConverter;
use serde_json::json;

#[test]
fn test_responses_simple_to_openai() {
    let req = json!({"model": "gpt-4o", "input": "说一句话"});
    let result = converter::request::responses_to_openai(&req, "gpt-4o");
    println!("Simple: {}", serde_json::to_string_pretty(&result).unwrap());

    assert_eq!(result["model"], "gpt-4o");
    let msgs = result["messages"].as_array().unwrap();
    assert_eq!(msgs.len(), 1);
    assert_eq!(msgs[0]["role"], "user");
    assert_eq!(msgs[0]["content"], "说一句话");
}

#[test]
fn test_responses_with_tools_to_openai() {
    let req = json!({
        "model": "gpt-4o",
        "input": "北京天气",
        "tools": [{
            "type": "function",
            "name": "get_weather",
            "description": "获取天气",
            "parameters": {"type": "object", "properties": {"location": {"type": "string"}}, "required": ["location"]}
        }]
    });
    let result = converter::request::responses_to_openai(&req, "gpt-4o");
    println!(
        "With tools: {}",
        serde_json::to_string_pretty(&result).unwrap()
    );

    let tools = result["tools"].as_array().unwrap();
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0]["type"], "function");
    assert_eq!(tools[0]["function"]["name"], "get_weather");
    assert!(tools[0]["function"]["parameters"].is_object());
}

#[test]
fn test_responses_to_openai_then_to_anthropic() {
    let req = json!({"model": "gpt-4o", "input": "说一句话"});
    let openai_body = converter::request::responses_to_openai(&req, "claude-sonnet-4-20250514");
    println!(
        "OpenAI: {}",
        serde_json::to_string_pretty(&openai_body).unwrap()
    );

    let anthropic_body =
        converter::request::openai_to_anthropic(&openai_body, "claude-sonnet-4-20250514");
    println!(
        "Anthropic: {}",
        serde_json::to_string_pretty(&anthropic_body).unwrap()
    );

    assert_eq!(anthropic_body["model"], "claude-sonnet-4-20250514");
    let msgs = anthropic_body["messages"].as_array().unwrap();
    assert_eq!(msgs.len(), 1);
    assert_eq!(msgs[0]["role"], "user");
    assert_eq!(msgs[0]["content"], "说一句话");
    assert!(anthropic_body["max_tokens"].as_i64().unwrap() > 0);
}

#[test]
fn test_responses_with_tools_to_anthropic() {
    let req = json!({
        "model": "gpt-4o",
        "input": "北京天气",
        "tools": [{
            "type": "function",
            "name": "get_weather",
            "description": "获取天气",
            "parameters": {"type": "object", "properties": {"location": {"type": "string"}}, "required": ["location"]}
        }]
    });
    let openai_body = converter::request::responses_to_openai(&req, "claude-sonnet-4-20250514");
    let anthropic_body =
        converter::request::openai_to_anthropic(&openai_body, "claude-sonnet-4-20250514");
    println!(
        "Anthropic with tools: {}",
        serde_json::to_string_pretty(&anthropic_body).unwrap()
    );

    let tools = anthropic_body["tools"].as_array().unwrap();
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0]["name"], "get_weather");
    assert!(tools[0]["input_schema"].is_object());
}

#[test]
fn test_responses_additional_tools_to_anthropic() {
    let fc = FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();
    let req = json!({
        "model": "gpt-5.6-sol",
        "input": [
            {"type": "additional_tools", "role": "developer", "tools": [
                {"type": "custom", "name": "exec", "description": "Run code", "format": {"type": "grammar", "syntax": "lark", "definition": "start: /.+/"}},
                {"type": "function", "name": "wait", "description": "Wait", "parameters": {"type": "object", "properties": {"seconds": {"type": "number"}}}}
            ]},
            {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "continue"}]}
        ]
    });
    let anthropic = fc.convert_request(&req, "claude-opus-4-6");
    let tools = anthropic["tools"].as_array().expect("additional_tools must be forwarded");
    assert_eq!(tools.len(), 2);
    let custom = tools.iter().find(|tool| tool["name"] == "exec").expect("custom tool");
    assert_eq!(custom["input_schema"]["properties"]["content"]["type"], "string");
    assert!(custom["description"].as_str().unwrap().contains("Format:"));
    assert!(tools.iter().any(|tool| tool["name"] == "wait"));
}

#[test]
fn test_responses_custom_tool_response_roundtrip() {
    let fc = FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();
    let req = json!({
        "model": "gpt-5.6-sol",
        "input": [{"type": "additional_tools", "tools": [
            {"type": "custom", "name": "exec", "description": "Run code"}
        ]}]
    });
    let _ = fc.convert_request(&req, "claude-opus-4-6");
    let response = json!({
        "id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-4-6",
        "content": [{"type": "tool_use", "id": "toolu_1", "name": "exec", "input": {"content": "return 1"}}],
        "stop_reason": "tool_use"
    });
    let responses = fc.convert_response(&response, "gpt-5.6-sol");
    let item = responses["output"].as_array().unwrap().iter().find(|item| item["type"] == "custom_tool_call").expect("custom output item");
    assert_eq!(item["call_id"], "toolu_1");
    assert_eq!(item["input"], "return 1");
    assert!(item.get("arguments").is_none());
}

#[test]
fn test_openai_response_to_responses_format() {
    let openai_resp = json!({
        "id": "chatcmpl-abc123",
        "object": "chat.completion",
        "model": "gpt-4o",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": "你好"}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
    });
    let result = converter::response::to_responses(&openai_resp, "gpt-4o");
    println!(
        "Responses format: {}",
        serde_json::to_string_pretty(&result).unwrap()
    );

    assert_eq!(result["object"], "response");
    assert_eq!(result["status"], "completed");
    let output = result["output"].as_array().unwrap();
    assert!(!output.is_empty());
    assert_eq!(output[0]["type"], "message");
    let content = output[0]["content"].as_array().unwrap();
    assert_eq!(content[0]["type"], "output_text");
    assert_eq!(content[0]["text"], "你好");
}

#[test]
fn test_anthropic_response_to_responses_format() {
    let anthropic_resp = json!({
        "id": "msg_abc123",
        "type": "message",
        "role": "assistant",
        "model": "claude-sonnet-4-20250514",
        "content": [{"type": "text", "text": "你好"}],
        "stop_reason": "end_turn",
        "usage": {"input_tokens": 10, "output_tokens": 5}
    });
    let result = converter::response::to_responses(&anthropic_resp, "claude-sonnet-4-20250514");
    println!(
        "Responses from anthropic: {}",
        serde_json::to_string_pretty(&result).unwrap()
    );

    assert_eq!(result["object"], "response");
    assert_eq!(result["status"], "completed");
    let output = result["output"].as_array().unwrap();
    assert!(!output.is_empty());
}

#[test]
fn test_stream_converter_openai_to_responses() {
    let mut conv = converter::stream::ChatCompletionsToResponsesStream::new();

    // 首个 chunk: role
    let chunk1 = b"data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n";
    let out1 = conv.process_chunk(chunk1);
    let text1: String = out1
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    println!("Stream out1:\n{}", text1);
    assert!(text1.contains("response.created"));
    assert!(text1.contains("response.in_progress"));

    // text delta
    let chunk2 = b"data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n";
    let out2 = conv.process_chunk(chunk2);
    let text2: String = out2
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    println!("Stream out2:\n{}", text2);
    assert!(text2.contains("response.output_text.delta"));
    assert!(text2.contains("hello"));

    // finish + DONE
    let chunk3 = b"data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n";
    let out3 = conv.process_chunk(chunk3);
    let text3: String = out3
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    println!("Stream out3:\n{}", text3);
    assert!(text3.contains("response.completed"));
}

#[test]
fn test_openai_response_with_reasoning_to_responses() {
    let openai_resp = json!({
        "id": "chatcmpl-abc123",
        "object": "chat.completion",
        "model": "o3",
        "choices": [{"index": 0, "message": {"role": "assistant", "reasoning_content": "Let me think...", "content": "The answer is 42"}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
    });
    let result = converter::response::to_responses(&openai_resp, "o3");
    println!(
        "Responses with reasoning: {}",
        serde_json::to_string_pretty(&result).unwrap()
    );

    assert_eq!(result["object"], "response");
    let output = result["output"].as_array().unwrap();
    assert_eq!(output[0]["type"], "message");
    let content = output[0]["content"].as_array().unwrap();
    // reasoning + text 两个 content part
    assert_eq!(content.len(), 2);
    // 第一个是 reasoning
    assert_eq!(content[0]["type"], "output_text");
    assert_eq!(content[0]["text"], "Let me think...");
    assert!(content[0]["annotations"]
        .as_array()
        .unwrap()
        .iter()
        .any(|a| a["type"] == "reasoning"));
    // 第二个是普通 text
    assert_eq!(content[1]["type"], "output_text");
    assert_eq!(content[1]["text"], "The answer is 42");
}

#[test]
fn test_anthropic_response_with_thinking_to_responses() {
    let anthropic_resp = json!({
        "id": "msg_abc123",
        "type": "message",
        "role": "assistant",
        "model": "claude-sonnet-4-20250514",
        "content": [{"type": "thinking", "thinking": "Let me think..."}, {"type": "text", "text": "你好"}],
        "stop_reason": "end_turn",
        "usage": {"input_tokens": 10, "output_tokens": 5}
    });
    let result = converter::response::to_responses(&anthropic_resp, "claude-sonnet-4-20250514");
    println!(
        "Responses from anthropic with thinking: {}",
        serde_json::to_string_pretty(&result).unwrap()
    );

    assert_eq!(result["object"], "response");
    let output = result["output"].as_array().unwrap();
    assert_eq!(output[0]["type"], "message");
    let content = output[0]["content"].as_array().unwrap();
    // thinking + text 两个 content part
    assert_eq!(content.len(), 2);
    assert_eq!(content[0]["text"], "Let me think...");
    assert_eq!(content[1]["text"], "你好");
}

#[test]
fn test_stream_converter_openai_with_reasoning_to_responses() {
    let mut conv = converter::stream::ChatCompletionsToResponsesStream::new();

    // role chunk
    let chunk1 = b"data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"model\":\"o3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n";
    let out1 = conv.process_chunk(chunk1);
    let text1: String = out1
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    assert!(text1.contains("response.created"));

    // reasoning_content delta
    let chunk2 = b"data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"model\":\"o3\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"Let me think\"},\"finish_reason\":null}]}\n\n";
    let out2 = conv.process_chunk(chunk2);
    let text2: String = out2
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    assert!(text2.contains("response.content_part.added"));
    assert!(text2.contains("response.output_text.delta"));
    assert!(text2.contains("Let me think"));
    // reasoning annotation
    assert!(text2.contains("reasoning"));

    // content delta (reasoning → text 切换)
    let chunk3 = b"data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"model\":\"o3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"42\"},\"finish_reason\":null}]}\n\n";
    let out3 = conv.process_chunk(chunk3);
    let text3: String = out3
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    // reasoning content part 应该被关闭，开始新的 text content part
    assert!(text3.contains("response.content_part.done"));
    assert!(text3.contains("response.content_part.added"));
    assert!(text3.contains("42"));

    // finish + DONE
    let chunk4 = b"data: {\"id\":\"chatcmpl-123\",\"object\":\"chat.completion.chunk\",\"model\":\"o3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n";
    let out4 = conv.process_chunk(chunk4);
    let text4: String = out4
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    assert!(text4.contains("response.completed"));
}

#[test]
fn test_stream_converter_anthropic_with_thinking_to_responses() {
    let mut conv = converter::stream::AnthropicToResponsesStream::new();

    // message_start
    let chunk1 = b"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-20250514\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n";
    let out1 = conv.process_chunk(chunk1);
    let text1: String = out1
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    assert!(text1.contains("response.created"));
    assert!(text1.contains("response.in_progress"));

    // thinking content_block_start
    let chunk2 = b"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n";
    let out2 = conv.process_chunk(chunk2);
    let text2: String = out2
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    assert!(text2.contains("response.output_item.added"));
    assert!(text2.contains("response.content_part.added"));
    assert!(text2.contains("reasoning")); // annotation

    // thinking_delta
    let chunk3 = b"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"Hmm\"}}\n\n";
    let out3 = conv.process_chunk(chunk3);
    let text3: String = out3
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    assert!(text3.contains("response.output_text.delta"));
    assert!(text3.contains("Hmm"));

    // text content_block_start (thinking → text 切换)
    let chunk4 = b"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n";
    let out4 = conv.process_chunk(chunk4);
    let text4: String = out4
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    // thinking content part 应该被关闭
    assert!(text4.contains("response.output_text.done"));
    assert!(text4.contains("response.content_part.done"));
    // text content part 开始
    assert!(text4.contains("response.content_part.added"));

    // text_delta
    let chunk5 = b"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n";
    let out5 = conv.process_chunk(chunk5);
    let text5: String = out5
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    assert!(text5.contains("response.output_text.delta"));
    assert!(text5.contains("hello"));

    // message_stop
    let chunk6 = b"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n";
    let out6 = conv.process_chunk(chunk6);
    let text6: String = out6
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    assert!(text6.contains("response.completed"));
    // 验证 response.completed 的 output 包含两个 content parts
    assert!(text6.contains("Hmm"));
    assert!(text6.contains("hello"));
}

// ==================== FormatConverter 抽象层测试 ====================

use model_bridge_lib::channel::config::ProviderType;
use model_bridge_lib::converter::{ApiFormat, FormatConverter};
use model_bridge_lib::proxy::context::InputFormat;

#[test]
fn test_format_converter_passthrough() {
    // 格式一致时应返回 None（透传）
    assert!(
        FormatConverter::from_formats(InputFormat::Anthropic, &ProviderType::Anthropic).is_none()
    );
    assert!(FormatConverter::from_formats(InputFormat::OpenAI, &ProviderType::Openai).is_none());
    assert!(
        FormatConverter::from_formats(InputFormat::Responses, &ProviderType::OpenaiResponses)
            .is_none()
    );
}

#[test]
fn test_format_converter_all_6_paths_request() {
    // 路径 1: Anthropic → OpenAI Chat
    let fc = FormatConverter::from_formats(InputFormat::Anthropic, &ProviderType::Openai).unwrap();
    assert_eq!(fc.from_format(), ApiFormat::Anthropic);
    assert_eq!(fc.to_format(), ApiFormat::OpenAIChat);
    let req = json!({"model": "claude-3", "messages": [{"role": "user", "content": "hi"}], "max_tokens": 100});
    let result = fc.convert_request(&req, "gpt-4o");
    assert_eq!(result["model"], "gpt-4o");
    assert!(result["messages"].is_array());

    // 路径 2: Anthropic → OpenAI Responses
    let fc = FormatConverter::from_formats(InputFormat::Anthropic, &ProviderType::OpenaiResponses)
        .unwrap();
    assert_eq!(fc.from_format(), ApiFormat::Anthropic);
    assert_eq!(fc.to_format(), ApiFormat::OpenAIResponses);
    let result = fc.convert_request(&req, "gpt-4o");
    assert!(result["input"].is_string() || result["input"].is_array());

    // 路径 3: OpenAI Chat → Anthropic
    let fc = FormatConverter::from_formats(InputFormat::OpenAI, &ProviderType::Anthropic).unwrap();
    assert_eq!(fc.from_format(), ApiFormat::OpenAIChat);
    assert_eq!(fc.to_format(), ApiFormat::Anthropic);
    let req = json!({"model": "gpt-4o", "messages": [{"role": "user", "content": "hi"}]});
    let result = fc.convert_request(&req, "claude-3");
    assert_eq!(result["model"], "claude-3");
    assert!(result["max_tokens"].is_number());

    // 路径 4: OpenAI Chat → OpenAI Responses
    let fc =
        FormatConverter::from_formats(InputFormat::OpenAI, &ProviderType::OpenaiResponses).unwrap();
    assert_eq!(fc.from_format(), ApiFormat::OpenAIChat);
    assert_eq!(fc.to_format(), ApiFormat::OpenAIResponses);
    let result = fc.convert_request(&req, "gpt-4o");
    assert!(result["input"].is_string() || result["input"].is_array());

    // 路径 5: OpenAI Responses → OpenAI Chat
    let fc = FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Openai).unwrap();
    assert_eq!(fc.from_format(), ApiFormat::OpenAIResponses);
    assert_eq!(fc.to_format(), ApiFormat::OpenAIChat);
    let req = json!({"model": "gpt-4o", "input": "hi"});
    let result = fc.convert_request(&req, "gpt-4o");
    assert!(result["messages"].is_array());

    // 路径 6: OpenAI Responses → Anthropic
    let fc =
        FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();
    assert_eq!(fc.from_format(), ApiFormat::OpenAIResponses);
    assert_eq!(fc.to_format(), ApiFormat::Anthropic);
    let req = json!({"model": "gpt-4o", "input": "hi"});
    let result = fc.convert_request(&req, "claude-3");
    assert!(result["max_tokens"].is_number());
}

#[test]
fn test_format_converter_all_6_paths_response() {
    // 路径 1 逆向: OpenAI Chat → Anthropic 响应
    let fc = FormatConverter::from_formats(InputFormat::Anthropic, &ProviderType::Openai).unwrap();
    let openai_resp = json!({
        "id": "chatcmpl-123", "model": "gpt-4o",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
    });
    let result = fc.convert_response(&openai_resp, "gpt-4o");
    assert_eq!(result["type"], "message");
    assert_eq!(result["role"], "assistant");

    // 路径 2 逆向: OpenAI Responses → Anthropic 响应
    let fc = FormatConverter::from_formats(InputFormat::Anthropic, &ProviderType::OpenaiResponses)
        .unwrap();
    let responses_resp = json!({
        "id": "resp_123", "object": "response", "model": "gpt-4o", "status": "completed",
        "output": [{"type": "message", "status": "completed", "role": "assistant", "content": [{"type": "output_text", "text": "hi", "annotations": []}]}],
        "usage": {"input_tokens": 10, "output_tokens": 5}
    });
    let result = fc.convert_response(&responses_resp, "gpt-4o");
    assert_eq!(result["type"], "message");
    assert_eq!(result["role"], "assistant");

    // 路径 3 逆向: Anthropic → OpenAI Chat 响应
    let fc = FormatConverter::from_formats(InputFormat::OpenAI, &ProviderType::Anthropic).unwrap();
    let anthropic_resp = json!({
        "id": "msg_123", "type": "message", "role": "assistant", "model": "claude-3",
        "content": [{"type": "text", "text": "hi"}],
        "stop_reason": "end_turn",
        "usage": {"input_tokens": 10, "output_tokens": 5}
    });
    let result = fc.convert_response(&anthropic_resp, "claude-3");
    assert!(result["choices"].is_array());

    // 路径 4 逆向: OpenAI Responses → OpenAI Chat 响应
    let fc =
        FormatConverter::from_formats(InputFormat::OpenAI, &ProviderType::OpenaiResponses).unwrap();
    let result = fc.convert_response(&responses_resp, "gpt-4o");
    assert!(result["choices"].is_array());

    // 路径 5 逆向: OpenAI Chat → OpenAI Responses 响应
    let fc = FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Openai).unwrap();
    let result = fc.convert_response(&openai_resp, "gpt-4o");
    assert_eq!(result["object"], "response");

    // 路径 6 逆向: Anthropic → OpenAI Responses 响应
    let fc =
        FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();
    let result = fc.convert_response(&anthropic_resp, "claude-3");
    assert_eq!(result["object"], "response");
}

#[test]
fn test_format_converter_stream_converter() {
    // 每条路径都应能创建流转换器
    let pairs = vec![
        (InputFormat::Anthropic, ProviderType::Openai),
        (InputFormat::Anthropic, ProviderType::OpenaiResponses),
        (InputFormat::OpenAI, ProviderType::Anthropic),
        (InputFormat::OpenAI, ProviderType::OpenaiResponses),
        (InputFormat::Responses, ProviderType::Openai),
        (InputFormat::Responses, ProviderType::Anthropic),
    ];

    for (input, provider) in pairs {
        let fc = FormatConverter::from_formats(input, &provider).unwrap();
        let _stream = fc.create_stream_converter();
        // 只要能创建不 panic 就行
    }
}

// ==================== EndpointConfig 向后兼容测试 ====================

use model_bridge_lib::channel::config::EndpointConfig;

#[test]
fn test_endpoint_config_new_format() {
    // 新格式：url 字段
    let yaml = "url: \"https://api.openai.com/v1/chat/completions\"";
    let ec: EndpointConfig = serde_yaml::from_str(yaml).unwrap();
    assert_eq!(ec.url, "https://api.openai.com/v1/chat/completions");
}

#[test]
fn test_endpoint_config_old_format_chat_completions_only() {
    // 旧格式：只有 chat_completions（openai / anthropic 渠道）
    let yaml = "chat_completions: \"https://api.anthropic.com/v1/messages\"";
    let ec: EndpointConfig = serde_yaml::from_str(yaml).unwrap();
    assert_eq!(ec.url, "https://api.anthropic.com/v1/messages");
}

#[test]
fn test_endpoint_config_old_format_with_responses() {
    // 旧格式：chat_completions + responses（openai_responses 渠道）
    // 应优先使用 responses 字段
    let yaml = r#"
chat_completions: "https://api.openai.com/v1/chat/completions"
responses: "https://api.openai.com/v1/responses"
"#;
    let ec: EndpointConfig = serde_yaml::from_str(yaml).unwrap();
    assert_eq!(ec.url, "https://api.openai.com/v1/responses");
}

#[test]
fn test_endpoint_config_serialize_is_new_format() {
    // 序列化始终输出新格式
    let ec = EndpointConfig {
        url: "https://api.openai.com/v1/chat/completions".to_string(),
    };
    let yaml = serde_yaml::to_string(&ec).unwrap();
    assert!(yaml.contains("url:"));
    assert!(!yaml.contains("chat_completions:"));
}

// ==================== Token 解析测试 ====================

use model_bridge_lib::audit::db::extract_tokens_from_json;

#[test]
fn test_extract_tokens_openai_chat_format() {
    // OpenAI Chat Completions 格式
    let body = json!({
        "usage": {
            "prompt_tokens": 100,
            "completion_tokens": 50,
            "total_tokens": 150,
            "prompt_tokens_details": { "cached_tokens": 80 }
        }
    });
    let tokens = extract_tokens_from_json(&body);
    assert_eq!(tokens.input, Some(100));
    assert_eq!(tokens.output, Some(50));
    assert_eq!(tokens.cache_read, Some(80));
}

#[test]
fn test_extract_tokens_anthropic_format() {
    // Anthropic Messages 格式
    let body = json!({
        "usage": {
            "input_tokens": 200,
            "output_tokens": 60,
            "cache_read_input_tokens": 150,
            "cache_creation_input_tokens": 30
        }
    });
    let tokens = extract_tokens_from_json(&body);
    assert_eq!(tokens.input, Some(200));
    assert_eq!(tokens.output, Some(60));
    assert_eq!(tokens.cache_read, Some(150));
    assert_eq!(tokens.cache_creation, Some(30));
}

#[test]
fn test_extract_tokens_responses_api_format() {
    // OpenAI Responses API 格式（usage 直接在顶层）
    let body = json!({
        "id": "resp_abc",
        "usage": {
            "input_tokens": 300,
            "output_tokens": 70,
            "input_tokens_details": { "cached_tokens": 250 },
            "output_tokens_details": { "reasoning_tokens": 10 }
        }
    });
    let tokens = extract_tokens_from_json(&body);
    assert_eq!(tokens.input, Some(300));
    assert_eq!(tokens.output, Some(70));
    assert_eq!(tokens.cache_read, Some(250));
}

#[test]
fn test_extract_tokens_responses_api_nested_format() {
    // Responses API response.completed 事件格式（usage 在 response 下）
    let body = json!({
        "type": "response.completed",
        "response": {
            "id": "resp_abc",
            "usage": {
                "input_tokens": 400,
                "output_tokens": 80,
                "input_tokens_details": { "cached_tokens": 350 }
            }
        }
    });
    let tokens = extract_tokens_from_json(&body);
    assert_eq!(tokens.input, Some(400));
    assert_eq!(tokens.output, Some(80));
    assert_eq!(tokens.cache_read, Some(350));
}

// ==================== 非 SSE 上游降级重放为 SSE（回归：audit id=14197）====================

// 场景：入口 /v1/responses（流式），路由到 Anthropic 渠道，但上游对流式请求
// 返回了非 SSE 的完整 JSON（如带 tool_use）。此前会导致客户端收到空响应。
// 修复后应把完整响应转成入口格式（Responses）并重放为完整 SSE 事件流。

#[test]
fn test_full_response_to_sse_anthropic_upstream_to_responses_entry() {
    let fc =
        FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();
    // 模拟 14197：Anthropic 完整 JSON（含 text block、stop_reason=tool_use）
    let upstream = json!({
        "id": "msg_vrtx_016eQd9pEvk5K5yXP27pfhEo",
        "type": "message",
        "role": "assistant",
        "model": "claude-opus-4-6",
        "content": [{"type": "text", "text": "Let me check the project structure."}],
        "stop_reason": "tool_use",
        "usage": {"input_tokens": 14326, "output_tokens": 213}
    });
    let sse = fc.full_response_to_sse(&upstream, "claude-opus-4-6");
    let text = String::from_utf8_lossy(&sse).to_string();
    println!("Replayed Responses SSE:\n{}", text);

    // 必须是完整的 Responses 事件序列，而非空
    assert!(!sse.is_empty(), "重放输出不能为空");
    assert!(text.contains("event: response.created"));
    assert!(text.contains("event: response.in_progress"));
    assert!(text.contains("event: response.output_item.added"));
    assert!(text.contains("event: response.content_part.added"));
    assert!(text.contains("event: response.output_text.delta"));
    assert!(text.contains("Let me check the project structure."));
    assert!(text.contains("event: response.output_text.done"));
    assert!(text.contains("event: response.content_part.done"));
    assert!(text.contains("event: response.output_item.done"));
    assert!(text.contains("event: response.completed"));

    // response.completed 必须是 Responses 格式（object=response, status=completed）
    let completed_line = text
        .lines()
        .filter(|l| l.starts_with("data:"))
        .map(|l| l.trim_start_matches("data:").trim())
        .filter_map(|d| serde_json::from_str::<serde_json::Value>(d).ok())
        .find(|v| v["type"] == "response.completed")
        .expect("必须有 response.completed 事件");
    assert_eq!(completed_line["response"]["object"], "response");
    // tool_use → completed
    assert_eq!(completed_line["response"]["status"], "completed");
    // usage 已换算为 Responses 格式
    assert_eq!(completed_line["response"]["usage"]["input_tokens"], 14326);
    assert_eq!(completed_line["response"]["usage"]["output_tokens"], 213);
}

#[test]
fn test_full_response_to_sse_anthropic_upstream_to_chat_entry() {
    // 入口 Chat，上游 Anthropic 非 SSE 完整响应 → 重放为 chat.completion.chunk 流
    let fc = FormatConverter::from_formats(InputFormat::OpenAI, &ProviderType::Anthropic).unwrap();
    let upstream = json!({
        "id": "msg_123", "type": "message", "role": "assistant", "model": "claude-3",
        "content": [{"type": "text", "text": "hi there"}],
        "stop_reason": "end_turn",
        "usage": {"input_tokens": 10, "output_tokens": 5}
    });
    let sse = fc.full_response_to_sse(&upstream, "claude-3");
    let text = String::from_utf8_lossy(&sse).to_string();
    println!("Replayed Chat SSE:\n{}", text);

    assert!(!sse.is_empty());
    assert!(text.contains("chat.completion.chunk"));
    assert!(text.contains("\"role\":\"assistant\""));
    assert!(text.contains("hi there"));
    assert!(text.contains("\"finish_reason\":\"stop\""));
    assert!(text.contains("data: [DONE]"));
}

#[test]
fn test_full_response_to_sse_openai_upstream_to_anthropic_entry() {
    // 入口 Anthropic，上游 OpenAI Chat 非 SSE 完整响应 → 重放为 Anthropic SSE
    let fc = FormatConverter::from_formats(InputFormat::Anthropic, &ProviderType::Openai).unwrap();
    let upstream = json!({
        "id": "chatcmpl-1", "object": "chat.completion", "model": "gpt-4o",
        "choices": [{"index": 0, "message": {"role": "assistant", "content": "hello"}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
    });
    let sse = fc.full_response_to_sse(&upstream, "gpt-4o");
    let text = String::from_utf8_lossy(&sse).to_string();
    println!("Replayed Anthropic SSE:\n{}", text);

    assert!(!sse.is_empty());
    assert!(text.contains("event: message_start"));
    assert!(text.contains("event: content_block_start"));
    assert!(text.contains("event: content_block_delta"));
    assert!(text.contains("hello"));
    assert!(text.contains("event: message_delta"));
    assert!(text.contains("event: message_stop"));
    assert!(text.contains("end_turn"));
}

// ==================== 流式 tool_use / 工具调用回归测试 ====================

/// 把转换器输出的字节块拼成一整段字符串。
fn collect_text(chunks: &[Vec<u8>]) -> String {
    chunks
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect()
}

/// 解析一段 SSE 文本里的所有 `data: {json}` 行为 JSON Value（跳过 [DONE]）。
fn parse_data_events(text: &str) -> Vec<serde_json::Value> {
    text.lines()
        .filter_map(|l| l.strip_prefix("data:"))
        .map(|d| d.trim())
        .filter(|d| *d != "[DONE]")
        .filter_map(|d| serde_json::from_str::<serde_json::Value>(d).ok())
        .collect()
}

#[test]
fn test_stream_anthropic_tool_use_to_responses() {
    let mut conv = converter::stream::AnthropicToResponsesStream::new();
    let mut all: Vec<Vec<u8>> = Vec::new();

    all.extend(conv.process_chunk(b"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-20250514\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n"));
    all.extend(conv.process_chunk(b"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_abc\",\"name\":\"get_weather\",\"input\":{}}}\n\n"));
    all.extend(conv.process_chunk(b"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"location\\\":\"}}\n\n"));
    all.extend(conv.process_chunk(b"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\" \\\"SF\\\"}\"}}\n\n"));
    all.extend(conv.process_chunk(
        b"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
    ));
    all.extend(conv.process_chunk(b"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":89}}\n\n"));
    all.extend(conv.process_chunk(b"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"));

    let text = collect_text(&all);
    println!("Anthropic tool_use → Responses:\n{}", text);
    let events = parse_data_events(&text);

    // response.output_item.added(function_call)
    let added = events
        .iter()
        .find(|e| e["type"] == "response.output_item.added" && e["item"]["type"] == "function_call")
        .expect("必须有 function_call 的 output_item.added");
    assert_eq!(added["item"]["name"], "get_weather");
    assert_eq!(added["item"]["call_id"], "toolu_abc");
    assert_eq!(added["item"]["arguments"], "");
    assert!(added["output_index"].as_i64().unwrap() >= 1);

    // function_call_arguments.delta 至少两条，拼接为完整 JSON
    let deltas: Vec<String> = events
        .iter()
        .filter(|e| e["type"] == "response.function_call_arguments.delta")
        .map(|e| e["delta"].as_str().unwrap_or("").to_string())
        .collect();
    assert_eq!(deltas.len(), 2, "应有两条参数增量");
    assert_eq!(deltas.concat(), "{\"location\": \"SF\"}");

    // function_call_arguments.done
    let done = events
        .iter()
        .find(|e| e["type"] == "response.function_call_arguments.done")
        .expect("必须有 function_call_arguments.done");
    assert_eq!(done["arguments"], "{\"location\": \"SF\"}");

    // output_item.done(function_call)
    let item_done = events
        .iter()
        .find(|e| e["type"] == "response.output_item.done" && e["item"]["type"] == "function_call")
        .expect("必须有 function_call 的 output_item.done");
    assert_eq!(item_done["item"]["name"], "get_weather");
    assert_eq!(item_done["item"]["call_id"], "toolu_abc");
    assert_eq!(item_done["item"]["arguments"], "{\"location\": \"SF\"}");
    assert_eq!(item_done["item"]["status"], "completed");

    // response.completed 的 output 含该 function_call
    let completed = events
        .iter()
        .find(|e| e["type"] == "response.completed")
        .expect("必须有 response.completed");
    let output = completed["response"]["output"].as_array().unwrap();
    let fc = output
        .iter()
        .find(|it| it["type"] == "function_call")
        .expect("completed.output 必须含 function_call");
    assert_eq!(fc["name"], "get_weather");
    assert_eq!(fc["arguments"], "{\"location\": \"SF\"}");
}

#[test]
fn test_stream_anthropic_tool_use_to_chat() {
    let mut conv = converter::stream::AnthropicToChatStream::new();
    let mut all: Vec<Vec<u8>> = Vec::new();

    all.extend(conv.process_chunk(b"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-20250514\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n"));
    all.extend(conv.process_chunk(b"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_abc\",\"name\":\"get_weather\",\"input\":{}}}\n\n"));
    all.extend(conv.process_chunk(b"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"location\\\":\"}}\n\n"));
    all.extend(conv.process_chunk(b"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\" \\\"SF\\\"}\"}}\n\n"));
    all.extend(conv.process_chunk(
        b"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
    ));
    all.extend(conv.process_chunk(b"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":89}}\n\n"));
    all.extend(conv.process_chunk(b"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"));

    let text = collect_text(&all);
    println!("Anthropic tool_use → Chat:\n{}", text);
    let events = parse_data_events(&text);

    // 首帧 tool_calls：含 id/name、arguments 为空
    let first = events
        .iter()
        .find(|e| {
            e["choices"][0]["delta"]["tool_calls"][0]["id"].is_string()
                && e["choices"][0]["delta"]["tool_calls"][0]["function"]["name"] == "get_weather"
        })
        .expect("必须有含 id/name 的首帧 tool_calls");
    let tc0 = &first["choices"][0]["delta"]["tool_calls"][0];
    assert_eq!(tc0["index"], 0);
    assert_eq!(tc0["id"], "toolu_abc");
    assert_eq!(tc0["type"], "function");
    assert_eq!(tc0["function"]["arguments"], "");

    // 后续 arguments 增量（无 id/name），拼接为完整 JSON
    let args: Vec<String> = events
        .iter()
        .filter_map(|e| {
            let tc = &e["choices"][0]["delta"]["tool_calls"][0];
            if tc["id"].is_null() && tc["function"]["arguments"].is_string() {
                Some(tc["function"]["arguments"].as_str().unwrap().to_string())
            } else {
                None
            }
        })
        .collect();
    assert_eq!(args.concat(), "{\"location\": \"SF\"}");

    // finish_reason 最终为 tool_calls
    let finish = events
        .iter()
        .filter_map(|e| e["choices"][0]["finish_reason"].as_str())
        .last()
        .expect("必须有 finish_reason");
    assert_eq!(finish, "tool_calls");
}

#[test]
fn test_stream_responses_function_call_to_chat() {
    let mut conv = converter::stream::ResponsesToChatStream::new();
    let mut all: Vec<Vec<u8>> = Vec::new();

    all.extend(conv.process_chunk(b"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-4o\",\"status\":\"in_progress\",\"output\":[]}}\n\n"));
    all.extend(conv.process_chunk(b"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"call_x\",\"call_id\":\"call_x\",\"name\":\"get_weather\",\"arguments\":\"\",\"status\":\"in_progress\"}}\n\n"));
    all.extend(conv.process_chunk(b"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"call_x\",\"output_index\":1,\"delta\":\"{\\\"location\\\":\"}\n\n"));
    all.extend(conv.process_chunk(b"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"call_x\",\"output_index\":1,\"delta\":\" \\\"SF\\\"}\"}\n\n"));
    all.extend(conv.process_chunk(b"event: response.function_call_arguments.done\ndata: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"call_x\",\"output_index\":1,\"arguments\":\"{\\\"location\\\": \\\"SF\\\"}\"}\n\n"));
    all.extend(conv.process_chunk(b"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-4o\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"call_x\",\"call_id\":\"call_x\",\"name\":\"get_weather\",\"arguments\":\"{\\\"location\\\": \\\"SF\\\"}\",\"status\":\"completed\"}]}}\n\n"));

    let text = collect_text(&all);
    println!("Responses function_call → Chat:\n{}", text);
    let events = parse_data_events(&text);

    // 首帧 tool_calls：含 id/name
    let first = events
        .iter()
        .find(|e| e["choices"][0]["delta"]["tool_calls"][0]["id"].is_string())
        .expect("必须有含 id 的首帧 tool_calls");
    let tc0 = &first["choices"][0]["delta"]["tool_calls"][0];
    assert_eq!(tc0["index"], 0);
    assert_eq!(tc0["id"], "call_x");
    assert_eq!(tc0["type"], "function");
    assert_eq!(tc0["function"]["name"], "get_weather");

    // arguments 增量拼接
    let args: Vec<String> = events
        .iter()
        .filter_map(|e| {
            let tc = &e["choices"][0]["delta"]["tool_calls"][0];
            if tc["id"].is_null() && tc["function"]["arguments"].is_string() {
                Some(tc["function"]["arguments"].as_str().unwrap().to_string())
            } else {
                None
            }
        })
        .collect();
    assert_eq!(args.concat(), "{\"location\": \"SF\"}");

    // finish_reason 为 tool_calls
    let finish = events
        .iter()
        .filter_map(|e| e["choices"][0]["finish_reason"].as_str())
        .last()
        .expect("必须有 finish_reason");
    assert_eq!(finish, "tool_calls");
}

#[test]
fn test_stream_chat_tool_calls_to_responses() {
    // provider 为 OpenAI Chat、入口为 Responses：Chat 的 tool_calls delta → Responses function_call 事件
    let mut conv = converter::stream::ChatCompletionsToResponsesStream::new();
    let mut all: Vec<Vec<u8>> = Vec::new();

    // 首帧：role
    all.extend(conv.process_chunk(b"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n"));
    // tool_call 首帧：带 id/name，空 arguments
    all.extend(conv.process_chunk(b"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_xyz\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n"));
    // 后续：仅 arguments 增量（无 id/name，靠 index 关联）
    all.extend(conv.process_chunk(b"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"location\\\":\"}}]},\"finish_reason\":null}]}\n\n"));
    all.extend(conv.process_chunk(b"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\" \\\"SF\\\"}\"}}]},\"finish_reason\":null}]}\n\n"));
    // 结束
    all.extend(conv.process_chunk(b"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"));
    // 流结束符：收尾事件（function_call_arguments.done / response.completed）在
    // finish_reason 之后才发出——上游 usage chunk 位于 finish_reason 与 [DONE]
    // 之间，必须等它到达才能带上 usage。生产路径另由 executor 的 finalize() 兜底。
    all.extend(conv.process_chunk(b"data: [DONE]\n\n"));

    let text = collect_text(&all);
    println!("Chat tool_calls → Responses:\n{}", text);
    let events = parse_data_events(&text);

    let added = events
        .iter()
        .find(|e| e["type"] == "response.output_item.added" && e["item"]["type"] == "function_call")
        .expect("必须有 function_call 的 output_item.added");
    assert_eq!(added["item"]["name"], "get_weather");
    assert_eq!(added["item"]["call_id"], "call_xyz");
    assert!(added["output_index"].as_i64().unwrap() >= 1);

    let deltas: Vec<String> = events
        .iter()
        .filter(|e| e["type"] == "response.function_call_arguments.delta")
        .map(|e| e["delta"].as_str().unwrap_or("").to_string())
        .collect();
    assert_eq!(deltas.concat(), "{\"location\": \"SF\"}");

    let done = events
        .iter()
        .find(|e| e["type"] == "response.function_call_arguments.done")
        .expect("必须有 function_call_arguments.done");
    assert_eq!(done["arguments"], "{\"location\": \"SF\"}");

    // response.completed 的 output 必须含该 function_call
    let completed = events
        .iter()
        .find(|e| e["type"] == "response.completed")
        .expect("必须有 response.completed");
    let output = completed["response"]["output"].as_array().expect("output 数组");
    assert!(output.iter().any(|o| o["type"] == "function_call"
        && o["call_id"] == "call_xyz"
        && o["arguments"] == "{\"location\": \"SF\"}"));
}

// ==================== Anthropic prompt caching 断点测试 ====================

/// 递归统计 Value 中 cache_control 键出现的次数。
fn count_cache_control(v: &serde_json::Value) -> usize {
    match v {
        serde_json::Value::Object(m) => {
            let here = if m.contains_key("cache_control") { 1 } else { 0 };
            here + m.values().map(count_cache_control).sum::<usize>()
        }
        serde_json::Value::Array(arr) => arr.iter().map(count_cache_control).sum(),
        _ => 0,
    }
}

#[test]
fn test_openai_to_anthropic_preserves_client_cache_control() {
    // Chat 请求：user 消息 content 为数组，其中一个 text part 带 cache_control
    let req = json!({
        "model": "gpt-4o",
        "messages": [{
            "role": "user",
            "content": [
                {"type": "text", "text": "前缀大段上下文", "cache_control": {"type": "ephemeral"}},
                {"type": "text", "text": "追加问题"}
            ]
        }]
    });
    let anthropic = converter::request::openai_to_anthropic(&req, "claude-sonnet-4-20250514");
    println!(
        "Preserved cache_control: {}",
        serde_json::to_string_pretty(&anthropic).unwrap()
    );

    let msgs = anthropic["messages"].as_array().unwrap();
    assert_eq!(msgs.len(), 1);
    let blocks = msgs[0]["content"].as_array().expect("content 应为数组以承载 cache_control");
    assert_eq!(blocks.len(), 2);
    // 第一个块保留 cache_control
    assert_eq!(blocks[0]["type"], "text");
    assert_eq!(blocks[0]["text"], "前缀大段上下文");
    assert_eq!(blocks[0]["cache_control"]["type"], "ephemeral");
    // 第二个块无 cache_control
    assert!(blocks[1].get("cache_control").is_none());
    // 客户端已带断点，转换后总数恰为 1
    assert_eq!(count_cache_control(&anthropic), 1);
}

#[test]
fn test_inject_cache_breakpoints_when_absent() {
    // 无 cache_control 的 Anthropic 请求：system 两块、tools 两个、messages 两条
    let mut body = json!({
        "model": "claude-sonnet-4-20250514",
        "max_tokens": 1024,
        "system": [
            {"type": "text", "text": "系统提示 A"},
            {"type": "text", "text": "系统提示 B"}
        ],
        "tools": [
            {"name": "tool_a", "input_schema": {"type": "object"}},
            {"name": "tool_b", "input_schema": {"type": "object"}}
        ],
        "messages": [
            {"role": "user", "content": [{"type": "text", "text": "第一轮问题"}]},
            {"role": "assistant", "content": [{"type": "text", "text": "第一轮回答"}]}
        ]
    });

    converter::request::inject_anthropic_cache_breakpoints(&mut body);
    println!(
        "Injected: {}",
        serde_json::to_string_pretty(&body).unwrap()
    );

    // system 最后一块打点，倒数第一块不打
    let system = body["system"].as_array().unwrap();
    assert!(system[0].get("cache_control").is_none());
    assert_eq!(system[1]["cache_control"]["type"], "ephemeral");

    // tools 最后一个打点
    let tools = body["tools"].as_array().unwrap();
    assert!(tools[0].get("cache_control").is_none());
    assert_eq!(tools[1]["cache_control"]["type"], "ephemeral");

    // messages：最后一条末块打点
    let msgs = body["messages"].as_array().unwrap();
    let last_blocks = msgs[1]["content"].as_array().unwrap();
    assert_eq!(
        last_blocks.last().unwrap()["cache_control"]["type"],
        "ephemeral"
    );

    // 断点总数 ≤ 4（这里应为 4：system + tools + 倒数第二条 user + 最后一条）
    let total = count_cache_control(&body);
    assert!(total <= 4, "断点总数不得超过 4，实际 {}", total);
    assert_eq!(total, 4);
}

#[test]
fn test_inject_cache_breakpoints_skips_when_present() {
    // 已带 cache_control 的 Anthropic 请求
    let mut body = json!({
        "model": "claude-sonnet-4-20250514",
        "max_tokens": 1024,
        "system": [
            {"type": "text", "text": "系统提示", "cache_control": {"type": "ephemeral"}}
        ],
        "messages": [
            {"role": "user", "content": [{"type": "text", "text": "问题"}]}
        ]
    });
    let before = count_cache_control(&body);
    assert_eq!(before, 1);

    converter::request::inject_anthropic_cache_breakpoints(&mut body);
    println!(
        "Idempotent: {}",
        serde_json::to_string_pretty(&body).unwrap()
    );

    // 幂等：不新增断点
    let after = count_cache_control(&body);
    assert_eq!(after, before, "已有 cache_control 时不应新增断点");
}

#[test]
fn test_anthropic_to_openai_preserves_cache_control() {
    // Anthropic 请求：system 带 cache_control
    let anthropic_in = json!({
        "model": "claude-sonnet-4-20250514",
        "max_tokens": 1024,
        "system": [
            {"type": "text", "text": "系统前缀", "cache_control": {"type": "ephemeral"}}
        ],
        "messages": [
            {"role": "user", "content": "你好"}
        ]
    });

    // Anthropic → Chat（中枢承载 cache_control）
    let chat = converter::request::anthropic_to_openai(&anthropic_in, "claude-sonnet-4-20250514");
    println!("Chat intermediate: {}", serde_json::to_string_pretty(&chat).unwrap());
    assert!(count_cache_control(&chat) >= 1, "中枢应承载 cache_control");

    // Chat → Anthropic（往返回来仍保留）
    let anthropic_out =
        converter::request::openai_to_anthropic(&chat, "claude-sonnet-4-20250514");
    println!(
        "Roundtrip Anthropic: {}",
        serde_json::to_string_pretty(&anthropic_out).unwrap()
    );

    let system = anthropic_out["system"].as_array().expect("system 应为数组");
    assert_eq!(system.last().unwrap()["cache_control"]["type"], "ephemeral");
    assert!(count_cache_control(&anthropic_out) >= 1);
}

// ==================== namespace 工具展平与还原测试 ====================

#[test]
fn test_namespace_tools_flatten_and_roundtrip() {
    let fc =
        FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();

    let req = json!({
        "model": "claude-sonnet-4-20250514",
        "input": "test",
        "tools": [
            {
                "type": "function",
                "name": "standalone_fn",
                "description": "A standalone function",
                "parameters": {"type": "object", "properties": {"x": {"type": "string"}}}
            },
            {
                "type": "namespace",
                "name": "mcp__tools",
                "description": "MCP tools namespace",
                "tools": [
                    {
                        "type": "function",
                        "name": "sub_a",
                        "description": "Subtool A",
                        "parameters": {"type": "object", "properties": {"a": {"type": "string"}}}
                    },
                    {
                        "type": "function",
                        "name": "sub_b",
                        "description": "Subtool B",
                        "parameters": {"type": "object", "properties": {"b": {"type": "integer"}}}
                    }
                ]
            }
        ]
    });

    let anthropic_req = fc.convert_request(&req, "claude-sonnet-4-20250514");
    println!(
        "Anthropic request: {}",
        serde_json::to_string_pretty(&anthropic_req).unwrap()
    );

    let tools = anthropic_req["tools"]
        .as_array()
        .expect("tools must be array");
    assert_eq!(tools.len(), 3, "1 standalone + 2 namespace subtools");

    let names: Vec<&str> = tools
        .iter()
        .map(|t| t["name"].as_str().unwrap())
        .collect();
    assert!(names.contains(&"standalone_fn"));
    assert!(names.contains(&"mcp__tools__sub_a"));
    assert!(names.contains(&"mcp__tools__sub_b"));

    let sub_a_tool = tools
        .iter()
        .find(|t| t["name"] == "mcp__tools__sub_a")
        .unwrap();
    assert_eq!(sub_a_tool["description"], "Subtool A");
    assert!(sub_a_tool["input_schema"].is_object());

    let anthropic_resp = json!({
        "id": "msg_test",
        "type": "message",
        "role": "assistant",
        "model": "claude-sonnet-4-20250514",
        "content": [
            {"type": "tool_use", "id": "toolu_1", "name": "mcp__tools__sub_a", "input": {"a": "hello"}}
        ],
        "stop_reason": "tool_use",
        "usage": {"input_tokens": 100, "output_tokens": 20}
    });

    let responses_resp = fc.convert_response(&anthropic_resp, "claude-sonnet-4-20250514");
    println!(
        "Responses response: {}",
        serde_json::to_string_pretty(&responses_resp).unwrap()
    );

    assert_eq!(responses_resp["object"], "response");
    let output = responses_resp["output"].as_array().unwrap();
    let fc_item = output
        .iter()
        .find(|o| o["type"] == "function_call")
        .expect("must have function_call");
    assert_eq!(fc_item["name"], "sub_a", "name should be restored to subtool name");
    assert_eq!(
        fc_item["namespace"], "mcp__tools",
        "namespace should be restored"
    );
    assert_eq!(fc_item["call_id"], "toolu_1");
}

#[test]
fn test_namespace_naming_sanitize() {
    let fc =
        FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();

    let long_name = "a".repeat(200);
    let req = json!({
        "model": "claude-sonnet-4-20250514",
        "input": "test",
        "tools": [
            {
                "type": "namespace",
                "name": "ns.with.dots",
                "description": "NS with special chars",
                "tools": [
                    {
                        "type": "function",
                        "name": "sub.tool",
                        "description": "Sub with dots",
                        "parameters": {"type": "object"}
                    }
                ]
            },
            {
                "type": "namespace",
                "name": &long_name,
                "description": "Long NS",
                "tools": [
                    {
                        "type": "function",
                        "name": "tool",
                        "description": "Tool in long ns",
                        "parameters": {"type": "object"}
                    }
                ]
            }
        ]
    });

    let anthropic_req = fc.convert_request(&req, "claude-sonnet-4-20250514");
    let tools = anthropic_req["tools"].as_array().unwrap();
    assert_eq!(tools.len(), 2);

    for tool in tools {
        let name = tool["name"].as_str().unwrap();
        assert!(
            name.len() <= 128,
            "tool name must be <= 128 chars, got {}",
            name.len()
        );
        assert!(
            name.chars()
                .all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-'),
            "tool name must match Anthropic pattern: {}",
            name
        );
    }

    let dotted = tools
        .iter()
        .find(|t| t["name"].as_str().unwrap().contains("ns_with_dots"))
        .expect("dotted namespace should be sanitized");
    assert!(dotted["name"]
        .as_str()
        .unwrap()
        .contains("sub_tool"));
}

#[test]
fn test_web_search_tool_preserved() {
    let fc =
        FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();

    let req = json!({
        "model": "claude-sonnet-4-20250514",
        "input": "search the web",
        "tools": [
            {"type": "function", "name": "my_fn", "parameters": {"type": "object"}},
            {"type": "web_search", "external_web_access": false}
        ]
    });

    let anthropic_req = fc.convert_request(&req, "claude-sonnet-4-20250514");
    println!(
        "Anthropic with web_search: {}",
        serde_json::to_string_pretty(&anthropic_req).unwrap()
    );

    let tools = anthropic_req["tools"].as_array().unwrap();
    assert_eq!(tools.len(), 2, "function + web_search must both be present");

    let ws = tools
        .iter()
        .find(|t| t["type"] == "web_search_20250305")
        .expect("web_search must be converted to Anthropic hosted tool");
    assert_eq!(ws["name"], "web_search");
    assert_eq!(ws["max_uses"], 5);
}

#[test]
fn test_cross_request_isolation() {
    let fc1 =
        FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();
    let fc2 =
        FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();

    let req1 = json!({
        "model": "claude-sonnet-4-20250514",
        "input": "test1",
        "tools": [{
            "type": "namespace",
            "name": "ns_a",
            "tools": [{"type": "function", "name": "tool1", "parameters": {"type": "object"}}]
        }]
    });

    let req2 = json!({
        "model": "claude-sonnet-4-20250514",
        "input": "test2",
        "tools": [{
            "type": "namespace",
            "name": "ns_b",
            "tools": [{"type": "function", "name": "tool2", "parameters": {"type": "object"}}]
        }]
    });

    let _ = fc1.convert_request(&req1, "claude-sonnet-4-20250514");
    let _ = fc2.convert_request(&req2, "claude-sonnet-4-20250514");

    let ns1 = fc1.ns_reverse().read();
    let ns2 = fc2.ns_reverse().read();

    assert!(
        ns1.contains_key("ns_a__tool1"),
        "fc1 should have ns_a__tool1"
    );
    assert!(
        !ns1.contains_key("ns_b__tool2"),
        "fc1 should NOT have ns_b__tool2 (isolation)"
    );
    assert!(
        ns2.contains_key("ns_b__tool2"),
        "fc2 should have ns_b__tool2"
    );
    assert!(
        !ns2.contains_key("ns_a__tool1"),
        "fc2 should NOT have ns_a__tool1 (isolation)"
    );
}

/// 真实形态的 Codex Responses 请求（含 namespace / custom / web_search 工具）
/// 经 Responses → Anthropic 转换后，所有声明的工具都必须保留。
///
/// 夹具为仓库内固定文件。此前该测试读取的是调试时遗留在 `/tmp/audit4149.json`
/// 的审计导出，在任何没有该文件的机器上都会失败，因此不具备可复现性。
#[test]
fn test_real_codex_request_preserves_tools() {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/codex_responses_request.json");
    let raw = std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("failed to read fixture {}: {e}", path.display()));
    let body: serde_json::Value =
        serde_json::from_str(&raw).expect("fixture must be valid JSON");

    let fc =
        FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();
    let anthropic_req = fc.convert_request(&body, "claude-sonnet-4-20250514");

    let tools = anthropic_req["tools"]
        .as_array()
        .expect("tools must be array");
    let total = tools.len();
    println!("Total Anthropic tools: {}", total);

    let declared_tools: Vec<&serde_json::Value> = body["input"]
        .as_array()
        .into_iter()
        .flat_map(|items| items.iter())
        .filter(|item| item["type"] == "additional_tools")
        .flat_map(|item| item["tools"].as_array().into_iter().flatten())
        .collect();
    let expected_total: usize = declared_tools
        .iter()
        .map(|tool| match tool["type"].as_str() {
            Some("namespace") => tool["tools"]
                .as_array()
                .map(|subtools| subtools.iter().filter(|sub| sub["name"].is_string()).count())
                .unwrap_or(0),
            Some("function") | Some("custom") | Some("web_search") | Some("web_search_preview") => 1,
            _ => 0,
        })
        .sum();
    assert_eq!(total, expected_total, "all declared Codex tools must reach Anthropic");

    let web_search_count = tools
        .iter()
        .filter(|t| t["type"] == "web_search_20250305")
        .count();
    let expected_web_search_count = declared_tools
        .iter()
        .filter(|tool| matches!(tool["type"].as_str(), Some("web_search") | Some("web_search_preview")))
        .count();
    assert_eq!(web_search_count, expected_web_search_count);

    let function_count = tools
        .iter()
        .filter(|t| t.get("type").is_none() || t["type"] == "null")
        .count();
    let expected_function_count = declared_tools
        .iter()
        .filter(|tool| matches!(tool["type"].as_str(), Some("function") | Some("custom")))
        .count();
    assert!(function_count >= expected_function_count);

    let ns_reverse = fc.ns_reverse().read();
    let expected_reverse_count = declared_tools
        .iter()
        .map(|tool| match tool["type"].as_str() {
            Some("namespace") => tool["tools"]
                .as_array()
                .map(|subtools| subtools.iter().filter(|sub| sub["name"].is_string()).count())
                .unwrap_or(0),
            Some("custom") => usize::from(tool["name"].is_string()),
            _ => 0,
        })
        .sum::<usize>();
    assert_eq!(ns_reverse.len(), expected_reverse_count);
}

#[test]
fn test_stream_anthropic_namespace_tool_use_to_responses() {
    let fc =
        FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();

    let req = json!({
        "model": "claude-sonnet-4-20250514",
        "input": "test",
        "tools": [{
            "type": "namespace",
            "name": "mcp__tools",
            "tools": [{
                "type": "function",
                "name": "sub_a",
                "description": "Sub A",
                "parameters": {"type": "object"}
            }]
        }]
    });
    let _ = fc.convert_request(&req, "claude-sonnet-4-20250514");

    let mut conv = fc.create_stream_converter();
    let mut all: Vec<Vec<u8>> = Vec::new();

    all.extend(conv.process_chunk(b"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-20250514\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n"));
    all.extend(conv.process_chunk(b"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_ns1\",\"name\":\"mcp__tools__sub_a\",\"input\":{}}}\n\n"));
    all.extend(conv.process_chunk(b"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\\\"hi\\\"}\"}}\n\n"));
    all.extend(conv.process_chunk(
        b"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
    ));
    all.extend(conv.process_chunk(b"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":10}}\n\n"));
    all.extend(conv.process_chunk(b"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"));

    let text: String = all
        .iter()
        .map(|b| String::from_utf8_lossy(b).to_string())
        .collect();
    println!("Stream namespace roundtrip:\n{}", text);

    let events: Vec<serde_json::Value> = text
        .lines()
        .filter_map(|l| l.strip_prefix("data:"))
        .map(|d| d.trim())
        .filter(|d| *d != "[DONE]")
        .filter_map(|d| serde_json::from_str::<serde_json::Value>(d).ok())
        .collect();

    let added = events
        .iter()
        .find(|e| {
            e["type"] == "response.output_item.added" && e["item"]["type"] == "function_call"
        })
        .expect("must have function_call output_item.added");
    assert_eq!(
        added["item"]["name"], "sub_a",
        "stream should restore subtool name"
    );
    assert_eq!(
        added["item"]["namespace"], "mcp__tools",
        "stream should include namespace field"
    );

    let done = events
        .iter()
        .find(|e| {
            e["type"] == "response.output_item.done" && e["item"]["type"] == "function_call"
        })
        .expect("must have function_call output_item.done");
    assert_eq!(done["item"]["name"], "sub_a");
    assert_eq!(done["item"]["namespace"], "mcp__tools");
    assert_eq!(done["item"]["arguments"], "{\"a\":\"hi\"}");

    let completed = events
        .iter()
        .find(|e| e["type"] == "response.completed")
        .expect("must have response.completed");
    let output = completed["response"]["output"].as_array().unwrap();
    let fc_item = output
        .iter()
        .find(|o| o["type"] == "function_call")
        .expect("completed output must have function_call");
    assert_eq!(fc_item["name"], "sub_a");
    assert_eq!(fc_item["namespace"], "mcp__tools");
}

#[test]
fn test_stream_anthropic_custom_tool_use_to_responses() {
    let fc = FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();
    let req = json!({
        "model": "gpt-5.6-sol",
        "input": [{"type": "additional_tools", "tools": [
            {"type": "custom", "name": "exec", "description": "Run code"}
        ]}]
    });
    let _ = fc.convert_request(&req, "claude-opus-4-6");
    let mut conv = fc.create_stream_converter();
    let mut all: Vec<Vec<u8>> = Vec::new();
    all.extend(conv.process_chunk(b"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-opus-4-6\",\"content\":[]}}\n\n"));
    all.extend(conv.process_chunk(b"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"exec\",\"input\":{}}}\n\n"));
    all.extend(conv.process_chunk(b"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"content\\\":\\\"return 1\\\"}\"}}\n\n"));
    all.extend(conv.process_chunk(b"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"));
    all.extend(conv.process_chunk(b"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n"));
    all.extend(conv.process_chunk(b"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"));
    let text = collect_text(&all);
    assert!(text.contains("\"type\":\"custom_tool_call\""));
    assert!(text.contains("\"input\":\"return 1\""));
}

#[test]
fn test_namespace_disambiguation() {
    let fc =
        FormatConverter::from_formats(InputFormat::Responses, &ProviderType::Anthropic).unwrap();

    let req = json!({
        "model": "claude-sonnet-4-20250514",
        "input": "test",
        "tools": [
            {
                "type": "namespace",
                "name": "ns",
                "tools": [
                    {"type": "function", "name": "tool", "parameters": {"type": "object"}},
                    {"type": "function", "name": "tool", "parameters": {"type": "object"}}
                ]
            }
        ]
    });

    let anthropic_req = fc.convert_request(&req, "claude-sonnet-4-20250514");
    let tools = anthropic_req["tools"].as_array().unwrap();
    assert_eq!(tools.len(), 2);

    let names: Vec<&str> = tools
        .iter()
        .map(|t| t["name"].as_str().unwrap())
        .collect();
    assert_eq!(names[0], "ns__tool");
    assert!(
        names[1].starts_with("ns__tool_"),
        "second tool should have disambiguation suffix: {}",
        names[1]
    );
}
